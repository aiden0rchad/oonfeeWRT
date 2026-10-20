// Package testutil supplies local protocol fixtures for controller tests.
package testutil

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

type SSHServer struct {
	Addr, Fingerprint string
	Authentications   atomic.Int64
}

// StartSSH optionally forwards HTTP on the same port, matching devices whose
// stored Host includes a nonstandard port for both bootstrap and ubus.
func StartSSH(t testing.TB, httpAddr string, run func(string, io.Reader, io.Writer, io.Writer) uint32) *SSHServer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &SSHServer{Addr: ln.Addr().String(), Fingerprint: ssh.FingerprintSHA256(signer.PublicKey())}
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
		s.Authentications.Add(1)
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	var connections sync.Map
	t.Cleanup(func() {
		ln.Close()
		connections.Range(func(conn, _ any) bool { conn.(net.Conn).Close(); return true })
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, true)
			go func() {
				defer connections.Delete(conn)
				defer conn.Close()
				reader := bufio.NewReader(conn)
				prefix, err := reader.Peek(4)
				if err != nil {
					return
				}
				if string(prefix) != "SSH-" {
					upstream, err := net.Dial("tcp", httpAddr)
					if err != nil {
						return
					}
					defer upstream.Close()
					go func() { _, _ = io.Copy(upstream, reader) }()
					_, _ = io.Copy(conn, upstream)
					return
				}
				sc, channels, requests, err := ssh.NewServerConn(&bufferedConn{conn, reader}, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer channel.Close()
						for request := range requests {
							var payload struct{ Command string }
							if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
								_ = request.Reply(false, nil)
								continue
							}
							_ = request.Reply(true, nil)
							status := run(payload.Command, channel, channel, channel.Stderr())
							_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	return s
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
