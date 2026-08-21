package topology

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxLLDPLinks = 4096

type lldpNamedValue struct {
	name string
	raw  json.RawMessage
}

type lldpInterfaceValue struct {
	port    string
	raw     json.RawMessage
	regular bool
}

// DecodeLLDPJSON decodes lldpd's irregular `json` format. Repeated named
// elements become arrays of one-key objects, while singletons remain objects.
// Only explicit LLDP observations with a local interface and remote MAC
// chassis ID become links.
func DecodeLLDPJSON(deviceID int64, raw []byte) ([]LLDPLink, error) {
	if deviceID <= 0 {
		return nil, errors.New("topology: LLDP device id must be positive")
	}
	if len(raw) == 0 || len(raw) > maxExecOutput || !utf8.Valid(raw) {
		return nil, fmt.Errorf("topology: LLDP JSON is empty, invalid, or exceeds %d bytes", maxExecOutput)
	}
	root, err := decodeLLDPObject(raw, "root")
	if err != nil {
		return nil, err
	}
	lldpRaw, ok := root["lldp"]
	if !ok {
		return nil, errors.New("topology: LLDP JSON has no lldp object")
	}
	interfaces, err := decodeLLDPInterfaces(lldpRaw)
	if err != nil {
		return nil, err
	}

	links := make([]LLDPLink, 0, len(interfaces))
	seen := map[string]bool{}
	observations := 0
	for _, iface := range interfaces {
		if !validInterfaceName(iface.port) {
			return nil, errors.New("topology: LLDP has an invalid local interface")
		}
		neighbor, err := decodeLLDPObject(iface.raw, "interface "+iface.port)
		if err != nil {
			return nil, err
		}
		viaRaw, ok := neighbor["via"]
		if !ok {
			return nil, fmt.Errorf("topology: LLDP interface %q has no protocol", iface.port)
		}
		via, err := decodeLLDPString(viaRaw, "interface "+iface.port+" protocol")
		if err != nil {
			return nil, err
		}
		if via != "LLDP" {
			return nil, fmt.Errorf("topology: interface %q was observed via an unsupported protocol", iface.port)
		}
		chassisRaw, ok := neighbor["chassis"]
		if !ok {
			return nil, fmt.Errorf("topology: LLDP interface %q has no chassis", iface.port)
		}
		ids, err := decodeLLDPChassisIDs(chassisRaw, iface.regular)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("topology: LLDP interface %q has an empty chassis", iface.port)
		}
		for _, idRaw := range ids {
			observations++
			if observations > maxLLDPLinks {
				return nil, fmt.Errorf("topology: LLDP JSON exceeds %d links", maxLLDPLinks)
			}
			id, err := decodeLLDPObject(idRaw, "chassis id")
			if err != nil {
				return nil, err
			}
			typeRaw, typeOK := id["type"]
			valueRaw, valueOK := id["value"]
			if !typeOK || !valueOK {
				return nil, fmt.Errorf("topology: LLDP interface %q chassis id requires type and value", iface.port)
			}
			idType, err := decodeLLDPString(typeRaw, "chassis id type")
			if err != nil {
				return nil, err
			}
			if idType != "mac" {
				return nil, fmt.Errorf("topology: LLDP interface %q chassis id is not a MAC", iface.port)
			}
			value, err := decodeLLDPString(valueRaw, "chassis id value")
			if err != nil {
				return nil, err
			}
			mac, err := canonicalMAC(value)
			if err != nil || !validRemoteChassisMAC(mac) {
				return nil, fmt.Errorf("topology: LLDP interface %q has invalid chassis MAC", iface.port)
			}
			key := iface.port + "\x00" + mac
			if seen[key] {
				continue
			}
			seen[key] = true
			links = append(links, LLDPLink{DeviceID: deviceID, Port: iface.port, RemoteMAC: mac})
		}
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].Port == links[j].Port {
			return links[i].RemoteMAC < links[j].RemoteMAC
		}
		return links[i].Port < links[j].Port
	})
	return links, nil
}

func decodeLLDPInterfaces(raw json.RawMessage) ([]lldpInterfaceValue, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("topology: LLDP lldp value is empty")
	}
	if trimmed[0] == '{' {
		lldp, err := decodeLLDPObject(trimmed, "lldp")
		if err != nil {
			return nil, err
		}
		interfacesRaw, ok := lldp["interface"]
		if !ok {
			return []lldpInterfaceValue{}, nil
		}
		entries, err := decodeLLDPFlex(interfacesRaw, "interface")
		if err != nil {
			return nil, err
		}
		out := make([]lldpInterfaceValue, 0, len(entries))
		for _, entry := range entries {
			out = append(out, lldpInterfaceValue{port: entry.name, raw: entry.raw})
		}
		return out, nil
	}
	if trimmed[0] != '[' {
		return nil, errors.New("topology: LLDP lldp value must be an object or array")
	}
	containers, err := decodeLLDPArray(trimmed, "lldp")
	if err != nil {
		return nil, err
	}
	if len(containers) != 1 {
		return nil, errors.New("topology: LLDP regular format requires one lldp container")
	}
	lldp, err := decodeLLDPObject(containers[0], "lldp container")
	if err != nil {
		return nil, err
	}
	interfacesRaw, ok := lldp["interface"]
	if !ok {
		return []lldpInterfaceValue{}, nil
	}
	elements, err := decodeLLDPArray(interfacesRaw, "interface")
	if err != nil {
		return nil, err
	}
	out := make([]lldpInterfaceValue, 0, len(elements))
	for i, element := range elements {
		iface, err := decodeLLDPObject(element, fmt.Sprintf("interface entry %d", i+1))
		if err != nil {
			return nil, err
		}
		nameRaw, ok := iface["name"]
		if !ok {
			return nil, fmt.Errorf("topology: LLDP interface entry %d has no name", i+1)
		}
		name, err := decodeLLDPString(nameRaw, "interface name")
		if err != nil {
			return nil, err
		}
		out = append(out, lldpInterfaceValue{port: name, raw: element, regular: true})
	}
	return out, nil
}

func decodeLLDPChassisIDs(raw json.RawMessage, regular bool) ([]json.RawMessage, error) {
	if regular {
		chassis, err := decodeLLDPArray(raw, "chassis")
		if err != nil {
			return nil, err
		}
		ids := make([]json.RawMessage, 0, len(chassis))
		for i, element := range chassis {
			member, err := decodeLLDPObject(element, fmt.Sprintf("chassis entry %d", i+1))
			if err != nil {
				return nil, err
			}
			idRaw, ok := member["id"]
			if !ok {
				return nil, fmt.Errorf("topology: LLDP chassis entry %d has no id", i+1)
			}
			values, err := decodeLLDPArray(idRaw, "chassis id")
			if err != nil {
				return nil, err
			}
			if len(values) != 1 {
				return nil, fmt.Errorf("topology: LLDP chassis entry %d requires one id", i+1)
			}
			ids = append(ids, values[0])
		}
		return ids, nil
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		object, err := decodeLLDPObject(trimmed, "chassis")
		if err != nil {
			return nil, err
		}
		if id, direct := object["id"]; direct {
			return []json.RawMessage{id}, nil
		}
	}
	chassis, err := decodeLLDPFlex(raw, "chassis")
	if err != nil {
		return nil, err
	}
	ids := make([]json.RawMessage, 0, len(chassis))
	for _, remote := range chassis {
		member, err := decodeLLDPObject(remote.raw, "chassis")
		if err != nil {
			return nil, err
		}
		id, ok := member["id"]
		if !ok {
			return nil, errors.New("topology: LLDP chassis has no id")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// decodeLLDPFlex handles lldpd json's object-for-one/array-for-many encoding.
func decodeLLDPFlex(raw json.RawMessage, label string) ([]lldpNamedValue, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("topology: LLDP %s is empty", label)
	}
	if trimmed[0] == '{' {
		object, err := decodeLLDPObject(trimmed, label)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]lldpNamedValue, 0, len(keys))
		for _, key := range keys {
			out = append(out, lldpNamedValue{name: key, raw: object[key]})
		}
		return out, nil
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("topology: LLDP %s must be an object or array", label)
	}
	elements, err := decodeLLDPArray(trimmed, label)
	if err != nil {
		return nil, err
	}
	out := make([]lldpNamedValue, 0, len(elements))
	for i, element := range elements {
		object, err := decodeLLDPObject(element, fmt.Sprintf("%s entry %d", label, i+1))
		if err != nil {
			return nil, err
		}
		if len(object) != 1 {
			return nil, fmt.Errorf("topology: LLDP %s array entry %d must contain one named object", label, i+1)
		}
		for name, value := range object {
			out = append(out, lldpNamedValue{name: name, raw: value})
		}
	}
	return out, nil
}

func decodeLLDPObject(raw []byte, label string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("topology: LLDP %s must be an object", label)
	}
	object := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("topology: LLDP %s: %w", label, err)
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("topology: LLDP %s has a non-string key", label)
		}
		if _, duplicate := object[key]; duplicate {
			return nil, fmt.Errorf("topology: LLDP %s repeats a key", label)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("topology: LLDP %s value: %w", label, err)
		}
		object[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("topology: LLDP %s: %w", label, err)
	}
	if err := requireLLDPEOF(decoder); err != nil {
		return nil, fmt.Errorf("topology: LLDP %s: %w", label, err)
	}
	if len(object) > maxLLDPLinks {
		return nil, fmt.Errorf("topology: LLDP %s exceeds %d entries", label, maxLLDPLinks)
	}
	return object, nil
}

func decodeLLDPArray(raw []byte, label string) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, fmt.Errorf("topology: LLDP %s must be an array", label)
	}
	var out []json.RawMessage
	for decoder.More() {
		if len(out) == maxLLDPLinks {
			return nil, fmt.Errorf("topology: LLDP %s exceeds %d entries", label, maxLLDPLinks)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("topology: LLDP %s: %w", label, err)
		}
		out = append(out, value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("topology: LLDP %s: %w", label, err)
	}
	if err := requireLLDPEOF(decoder); err != nil {
		return nil, fmt.Errorf("topology: LLDP %s: %w", label, err)
	}
	return out, nil
}

func decodeLLDPString(raw []byte, label string) (string, error) {
	var value string
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&value); err != nil || strings.TrimSpace(value) != value || value == "" {
		return "", fmt.Errorf("topology: LLDP %s must be a non-empty string", label)
	}
	if err := requireLLDPEOF(decoder); err != nil {
		return "", fmt.Errorf("topology: LLDP %s: %w", label, err)
	}
	return value, nil
}

func requireLLDPEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("has trailing JSON")
		}
		return err
	}
	return nil
}

func validRemoteChassisMAC(raw string) bool {
	mac, err := net.ParseMAC(raw)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
		return false
	}
	for _, octet := range mac {
		if octet != 0 {
			return true
		}
	}
	return false
}
