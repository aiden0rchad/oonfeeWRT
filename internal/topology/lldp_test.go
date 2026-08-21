package topology

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeLLDPJSONSingletonAndCanonicalMAC(t *testing.T) {
	raw := []byte(`{
  "lldp": {"interface": {"lan4": {
    "via": "LLDP", "rid": "1", "age": "0 day, 00:00:59",
    "chassis": {"core-switch": {
      "descr": "switch", "id": {"type": "mac", "value": "AA-BB-CC-DD-EE-01"}
    }},
    "port": {"descr": "42", "id": {"type": "local", "value": "42"}}
  }}}
}`)
	got, err := DecodeLLDPJSON(7, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []LLDPLink{{DeviceID: 7, Port: "lan4", RemoteMAC: "aa:bb:cc:dd:ee:01"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %#v, want %#v", got, want)
	}
}

func TestDecodeLLDPJSONArrayVariantsAreStableAndDeduplicated(t *testing.T) {
	raw := []byte(`{"lldp":{"interface":[
  {"lan3":{"via":"LLDP","chassis":{"hall-ap":{"id":{"type":"mac","value":"02:00:00:00:00:03"}}}}},
  {"lan1":{"via":"LLDP","chassis":[
    {"office-ap":{"id":{"type":"mac","value":"02:00:00:00:00:02"}}},
    {"printer":{"id":{"type":"mac","value":"02:00:00:00:00:01"}}}
  ]}},
  {"lan3":{"via":"LLDP","chassis":{"duplicate":{"id":{"type":"mac","value":"02:00:00:00:00:03"}}}}}
]}}`)
	got, err := DecodeLLDPJSON(9, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []LLDPLink{
		{DeviceID: 9, Port: "lan1", RemoteMAC: "02:00:00:00:00:01"},
		{DeviceID: 9, Port: "lan1", RemoteMAC: "02:00:00:00:00:02"},
		{DeviceID: 9, Port: "lan3", RemoteMAC: "02:00:00:00:00:03"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %#v, want %#v", got, want)
	}
}

func TestDecodeLLDPJSONRegularArrayVariant(t *testing.T) {
	raw := []byte(`{"lldp":[{"interface":[
  {"name":"lan2","via":"LLDP","chassis":[
    {"name":"edge-switch","id":[{"type":"mac","value":"02:00:00:00:00:12"}]}
  ],"port":[{"id":[{"type":"ifname","value":"ethernet1"}]}]}
]}]}`)
	got, err := DecodeLLDPJSON(4, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []LLDPLink{{DeviceID: 4, Port: "lan2", RemoteMAC: "02:00:00:00:00:12"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %#v, want %#v", got, want)
	}
}

func TestDecodeLLDPJSONCompactChassisWithoutSystemName(t *testing.T) {
	raw := []byte(`{"lldp":{"interface":{"lan1":{"via":"LLDP","chassis":{
  "id":{"type":"mac","value":"02:00:00:00:00:11"},"descr":"unnamed peer"
}}}}}`)
	got, err := DecodeLLDPJSON(3, raw)
	if err != nil || len(got) != 1 || got[0].RemoteMAC != "02:00:00:00:00:11" {
		t.Fatalf("links = %#v, err %v", got, err)
	}
}

func TestDecodeLLDPJSONAcceptsDemonstratedEmptyOutput(t *testing.T) {
	for _, raw := range []string{
		`{"lldp":{}}`,
		`{"lldp":{"interface":{}}}`,
		`{"lldp":{"interface":[]}}`,
		`{"lldp":[{}]}`,
		`{"lldp":[{"interface":[]}]}`,
	} {
		got, err := DecodeLLDPJSON(1, []byte(raw))
		if err != nil || len(got) != 0 {
			t.Errorf("%s: got %#v, err %v", raw, got, err)
		}
	}
}

func TestDecodeLLDPJSONRejectsAmbiguousOrUnusableRows(t *testing.T) {
	validChassis := `"chassis":{"peer":{"id":{"type":"mac","value":"02:00:00:00:00:01"}}}`
	tests := map[string]string{
		"invalid device":         `{"lldp":{}}`,
		"missing lldp":           `{}`,
		"scalar lldp":            `{"lldp":true}`,
		"null interfaces":        `{"lldp":{"interface":null}}`,
		"ambiguous array item":   `{"lldp":{"interface":[{"lan1":{},"lan2":{}}]}}`,
		"invalid local port":     `{"lldp":{"interface":{"lan/1":{"via":"LLDP",` + validChassis + `}}}}`,
		"missing protocol":       `{"lldp":{"interface":{"lan1":{` + validChassis + `}}}}`,
		"non-LLDP protocol":      `{"lldp":{"interface":{"lan1":{"via":"CDPv2",` + validChassis + `}}}}`,
		"missing chassis":        `{"lldp":{"interface":{"lan1":{"via":"LLDP"}}}}`,
		"empty chassis":          `{"lldp":{"interface":{"lan1":{"via":"LLDP","chassis":{}}}}`,
		"missing chassis id":     `{"lldp":{"interface":{"lan1":{"via":"LLDP","chassis":{"peer":{}}}}}}`,
		"non-MAC chassis id":     `{"lldp":{"interface":{"lan1":{"via":"LLDP","chassis":{"peer":{"id":{"type":"local","value":"switch"}}}}}}}`,
		"malformed chassis MAC":  `{"lldp":{"interface":{"lan1":{"via":"LLDP","chassis":{"peer":{"id":{"type":"mac","value":"not-a-mac"}}}}}}}`,
		"multicast chassis MAC":  `{"lldp":{"interface":{"lan1":{"via":"LLDP","chassis":{"peer":{"id":{"type":"mac","value":"01:00:5e:00:00:01"}}}}}}}`,
		"zero chassis MAC":       `{"lldp":{"interface":{"lan1":{"via":"LLDP","chassis":{"peer":{"id":{"type":"mac","value":"00:00:00:00:00:00"}}}}}}}`,
		"duplicate relevant key": `{"lldp":{"interface":{"lan1":{"via":"LLDP","via":"LLDP",` + validChassis + `}}}}`,
		"regular missing name":   `{"lldp":[{"interface":[{"via":"LLDP","chassis":[]}]}]}`,
		"regular multiple ids":   `{"lldp":[{"interface":[{"name":"lan1","via":"LLDP","chassis":[{"id":[{"type":"mac","value":"02:00:00:00:00:01"},{"type":"mac","value":"02:00:00:00:00:02"}]}]}]}]}`,
		"trailing JSON":          `{"lldp":{}} {}`,
	}
	for name, raw := range tests {
		deviceID := int64(1)
		if name == "invalid device" {
			deviceID = 0
		}
		if _, err := DecodeLLDPJSON(deviceID, []byte(raw)); err == nil {
			t.Errorf("%s: malformed or ambiguous LLDP JSON accepted", name)
		}
	}
}

func TestDecodeLLDPJSONIsBounded(t *testing.T) {
	raw := append([]byte(`{"lldp":{"padding":"`), bytes.Repeat([]byte("x"), maxExecOutput)...)
	raw = append(raw, []byte(`"}}`)...)
	if _, err := DecodeLLDPJSON(1, raw); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized LLDP JSON accepted: %v", err)
	}
}

func TestDecodeLLDPJSONBoundsObservationCount(t *testing.T) {
	var raw bytes.Buffer
	raw.WriteString(`{"lldp":{"interface":[`)
	for i := 0; i <= maxLLDPLinks; i++ {
		if i > 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, `{"lan1":{"via":"LLDP","chassis":{"peer":{"id":{"type":"mac","value":"02:00:00:%02x:%02x:%02x"}}}}}`,
			(i>>16)&0xff, (i>>8)&0xff, i&0xff)
	}
	raw.WriteString(`]}}`)
	if raw.Len() >= maxExecOutput {
		t.Fatalf("fixture %d bytes exceeds byte bound", raw.Len())
	}
	if _, err := DecodeLLDPJSON(1, raw.Bytes()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("excessive LLDP observation count accepted: %v", err)
	}
}
