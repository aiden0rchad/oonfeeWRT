package ubus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Batch sends several invocations in one HTTP round trip and returns results in
// request order.
//
// Batching amortises *transport*, not device work. Measured on class A: 550
// calls in one 65KB request were accepted with per-call cost flat at ~0.5ms
// from ~10 calls up — but a focused poll is still ~92% iwinfo driver time, so
// batching a slow call does not make it fast. Chunk on bytes rather than on
// count, because the ceiling is request size.
//
// Per-call failures are reported in Result.Err; the returned error is non-nil
// only when the whole exchange failed.
func (c *Client) Batch(ctx context.Context, calls []Invocation) ([]Result, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]Result, 0, len(calls))
	for start := 0; start < len(calls); {
		end, payload, err := c.buildChunk(calls, start)
		if err != nil {
			return nil, err
		}
		res, err := c.sendChunk(ctx, calls[start:end], payload)
		if err != nil {
			return nil, err
		}
		out = append(out, res...)
		start = end
	}
	return out, nil
}

// buildChunk greedily packs invocations until the encoded body would exceed
// maxBatchBytes. At least one call is always included, so an oversized single
// call fails loudly on the wire rather than looping here forever.
func (c *Client) buildChunk(calls []Invocation, start int) (int, []byte, error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()

	reqs := make([]rpcRequest, 0, len(calls)-start)
	var encoded []byte
	end := start
	for i := start; i < len(calls); i++ {
		args := calls[i].Args
		if args == nil {
			args = struct{}{}
		}
		c.mu.Lock()
		c.nextID++
		id := c.nextID
		c.mu.Unlock()

		candidate := append(reqs, rpcRequest{
			JSONRPC: "2.0", ID: id, Method: "call",
			Params: []any{session, calls[i].Object, calls[i].Method, args},
		})
		buf, err := json.Marshal(candidate)
		if err != nil {
			return 0, nil, err
		}
		if len(buf) > maxBatchBytes && i > start {
			break
		}
		reqs, encoded, end = candidate, buf, i+1
		if len(buf) > maxBatchBytes {
			break // single oversized call: send it alone and let the device answer
		}
	}
	return end, encoded, nil
}

func (c *Client) sendChunk(ctx context.Context, calls []Invocation, payload []byte) ([]Result, error) {
	var resps []rpcResponse
	if err := c.postRaw(ctx, payload, &resps); err != nil {
		return nil, err
	}
	if len(resps) != len(calls) {
		return nil, &ProtocolError{Code: 0, Message: fmt.Sprintf(
			"batch: sent %d calls, got %d responses", len(calls), len(resps))}
	}

	results := make([]Result, len(calls))
	needsRelogin := false
	for i := range resps {
		status, data, err := decodeFrame(calls[i].Object, calls[i].Method, &resps[i])
		results[i] = Result{Status: status, Data: data}
		switch {
		case err != nil:
			results[i].Err = err
			var d *DeniedError
			if errors.As(err, &d) {
				needsRelogin = true
			}
		case status != StatusOK:
			results[i].Err = &StatusError{
				Object: calls[i].Object, Method: calls[i].Method, Status: status}
		}
	}

	// A dead session denies every call in the batch at once. Re-login once and
	// resend, mirroring Call's policy — but never inside a confirm window,
	// where a token refresh guarantees the device reverts.
	if needsRelogin && allDenied(results) {
		c.mu.Lock()
		inWindow := c.confirmWindow
		user, pass := c.user, c.pass
		c.mu.Unlock()
		if !inWindow && user != "" {
			if err := c.Login(ctx, user, pass); err == nil {
				end, fresh, err := c.buildChunk(calls, 0)
				// The end index is the point. buildChunk packs to a byte
				// budget, and the fresh session's ids can be WIDER than the
				// ones this chunk was packed with — when they straddle a
				// power-of-ten boundary, the rebuild holds one call fewer.
				//
				// Discarding `end` posted that short payload anyway. The
				// device then RAN N-1 calls, the length check rejected the
				// reply, and the original results were returned — so the
				// writes landed while the caller was told every one was
				// denied, and Retried stayed false, which makes IsPermanent
				// report a permanent ACL gap as transient.
				if err != nil || end != len(calls) {
					return results, nil
				}
				retry, err := c.resendChunk(ctx, calls, fresh)
				if err == nil {
					return retry, nil
				}
			}
		}
	}
	return results, nil
}

// resendChunk is sendChunk without the re-login branch, so a retry can never
// recurse into another retry.
func (c *Client) resendChunk(ctx context.Context, calls []Invocation, payload []byte) ([]Result, error) {
	var resps []rpcResponse
	if err := c.postRaw(ctx, payload, &resps); err != nil {
		return nil, err
	}
	if len(resps) != len(calls) {
		return nil, &ProtocolError{Code: 0, Message: "batch retry: length mismatch"}
	}
	results := make([]Result, len(calls))
	for i := range resps {
		status, data, err := decodeFrame(calls[i].Object, calls[i].Method, &resps[i])
		results[i] = Result{Status: status, Data: data, Err: err}
		if err == nil && status != StatusOK {
			results[i].Err = &StatusError{
				Object: calls[i].Object, Method: calls[i].Method, Status: status}
		}
		var d *DeniedError
		if errors.As(results[i].Err, &d) {
			d.Retried = true
		}
	}
	return results, nil
}

func allDenied(rs []Result) bool {
	for _, r := range rs {
		var d *DeniedError
		if !errors.As(r.Err, &d) {
			return false
		}
	}
	return len(rs) > 0
}

// Decode unmarshals a batch result's payload.
func (r Result) Decode(out any) error {
	if r.Err != nil {
		return r.Err
	}
	if len(r.Data) == 0 {
		return nil
	}
	return json.Unmarshal(r.Data, out)
}
