// Package portablebackup creates and extracts bounded, encrypted controller
// backup artifacts. It never opens the controller database or contacts routers.
package portablebackup

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	formatName          = "oonfeewrt-portable-backup"
	formatVersion       = 1
	headerSize          = 128
	chunkSize           = 1 << 20
	headerTagSize       = chacha20poly1305.Overhead
	maxPortableKeyBytes = 4096
	maxManifestBytes    = 64 << 10
	MaxDatabaseBytes    = 8 << 30
	maxArtifactBytes    = headerSize + maxPortableKeyBytes + 2*headerTagSize +
		maxManifestBytes + MaxDatabaseBytes + (MaxDatabaseBytes/chunkSize)*headerTagSize
	maxControllerVersion  = 128
	maxSchemaVersion      = math.MaxInt32
	databaseMemberName    = "controller.db"
	portableKeyMemberName = "portable-key.json"
	manifestMemberName    = "manifest.json"
	contentKeyDomain      = "portable-backup/content-key/v1"
	headerAADDomain       = "oonfeewrt/portable-backup/header/v1\x00"
	recordAADDomain       = "oonfeewrt/portable-backup/record/v1\x00"
)

var (
	magic             = [8]byte{'O', 'O', 'N', 'F', 'E', 'E', 'B', 'K'}
	ErrAuthentication = errors.New("portable backup: authentication failed")
)

// Metadata is authenticated inside the encrypted manifest.
type Metadata struct {
	ControllerVersion string
	SchemaVersion     int
	CreatedAt         time.Time
}

// Member describes one fixed artifact member.
type Member struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the authenticated, versioned restore preview.
type Manifest struct {
	Format            string    `json:"format"`
	Version           int       `json:"version"`
	CreatedAt         time.Time `json:"created_at"`
	ControllerVersion string    `json:"controller_version"`
	SchemaVersion     int       `json:"schema_version"`
	Database          Member    `json:"database"`
	PortableKey       Member    `json:"portable_key"`
}

type diskManifest struct {
	Format            string `json:"format"`
	Version           int    `json:"version"`
	CreatedAt         string `json:"created_at"`
	ControllerVersion string `json:"controller_version"`
	SchemaVersion     int    `json:"schema_version"`
	Database          Member `json:"database"`
	PortableKey       Member `json:"portable_key"`
}

type artifactHeader struct {
	PortableLen uint32
	ManifestLen uint32
	DatabaseLen uint64
	Chunks      uint32
	Salt        [32]byte
	NonceSeed   [16]byte
}

func encodeHeader(h artifactHeader) []byte {
	out := make([]byte, headerSize)
	copy(out[:8], magic[:])
	binary.BigEndian.PutUint16(out[8:10], formatVersion)
	binary.BigEndian.PutUint32(out[12:16], headerSize)
	binary.BigEndian.PutUint32(out[16:20], chunkSize)
	binary.BigEndian.PutUint32(out[20:24], h.PortableLen)
	binary.BigEndian.PutUint32(out[24:28], h.ManifestLen)
	binary.BigEndian.PutUint64(out[32:40], h.DatabaseLen)
	binary.BigEndian.PutUint32(out[40:44], h.Chunks)
	copy(out[48:80], h.Salt[:])
	copy(out[80:96], h.NonceSeed[:])
	return out
}

func decodeHeader(data []byte) (artifactHeader, error) {
	if len(data) != headerSize || !bytes.Equal(data[:8], magic[:]) {
		return artifactHeader{}, errors.New("portable backup: invalid file signature")
	}
	if binary.BigEndian.Uint16(data[8:10]) != formatVersion {
		return artifactHeader{}, errors.New("portable backup: unsupported format version")
	}
	if binary.BigEndian.Uint16(data[10:12]) != 0 ||
		binary.BigEndian.Uint32(data[12:16]) != headerSize ||
		binary.BigEndian.Uint32(data[16:20]) != chunkSize ||
		!allZero(data[28:32]) || !allZero(data[44:48]) || !allZero(data[96:]) {
		return artifactHeader{}, errors.New("portable backup: unsupported header fields")
	}
	h := artifactHeader{
		PortableLen: binary.BigEndian.Uint32(data[20:24]),
		ManifestLen: binary.BigEndian.Uint32(data[24:28]),
		DatabaseLen: binary.BigEndian.Uint64(data[32:40]),
		Chunks:      binary.BigEndian.Uint32(data[40:44]),
	}
	copy(h.Salt[:], data[48:80])
	copy(h.NonceSeed[:], data[80:96])
	if h.PortableLen == 0 || h.PortableLen > maxPortableKeyBytes {
		return artifactHeader{}, errors.New("portable backup: portable key length is out of range")
	}
	if h.ManifestLen == 0 || h.ManifestLen > maxManifestBytes {
		return artifactHeader{}, errors.New("portable backup: manifest length is out of range")
	}
	if h.DatabaseLen == 0 || h.DatabaseLen > MaxDatabaseBytes {
		return artifactHeader{}, errors.New("portable backup: database length is out of range")
	}
	if h.Chunks != databaseChunks(h.DatabaseLen) {
		return artifactHeader{}, errors.New("portable backup: database chunk count is inconsistent")
	}
	if allZero(h.Salt[:]) || allZero(h.NonceSeed[:]) {
		return artifactHeader{}, errors.New("portable backup: invalid key salt or nonce seed")
	}
	return h, nil
}

func databaseChunks(size uint64) uint32 {
	return uint32((size + chunkSize - 1) / chunkSize)
}

func artifactSize(h artifactHeader) uint64 {
	return headerSize + uint64(h.PortableLen) + headerTagSize +
		uint64(h.ManifestLen) + headerTagSize + h.DatabaseLen +
		uint64(h.Chunks)*headerTagSize
}

func validateMetadata(meta Metadata) (Metadata, error) {
	if meta.ControllerVersion == "" || len(meta.ControllerVersion) > maxControllerVersion ||
		!utf8.ValidString(meta.ControllerVersion) || strings.TrimSpace(meta.ControllerVersion) == "" {
		return Metadata{}, errors.New("portable backup: controller version is invalid")
	}
	for _, r := range meta.ControllerVersion {
		if unicode.IsControl(r) {
			return Metadata{}, errors.New("portable backup: controller version contains control characters")
		}
	}
	if meta.SchemaVersion < 1 || meta.SchemaVersion > maxSchemaVersion {
		return Metadata{}, errors.New("portable backup: schema version is out of range")
	}
	if meta.CreatedAt.IsZero() || meta.CreatedAt.Year() < 1 || meta.CreatedAt.Year() > 9999 {
		return Metadata{}, errors.New("portable backup: creation time is invalid")
	}
	meta.CreatedAt = meta.CreatedAt.UTC()
	return meta, nil
}

func marshalManifest(meta Metadata, databaseSize uint64, databaseHash [32]byte,
	portable []byte) ([]byte, Manifest, error) {
	disk := diskManifest{
		Format:            formatName,
		Version:           formatVersion,
		CreatedAt:         meta.CreatedAt.Format(time.RFC3339Nano),
		ControllerVersion: meta.ControllerVersion,
		SchemaVersion:     meta.SchemaVersion,
		Database: Member{
			Name: databaseMemberName, Size: databaseSize, SHA256: hex.EncodeToString(databaseHash[:]),
		},
		PortableKey: Member{
			Name: portableKeyMemberName, Size: uint64(len(portable)), SHA256: sha256Hex(portable),
		},
	}
	data, err := json.Marshal(disk)
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("portable backup: encode manifest: %w", err)
	}
	if len(data) == 0 || len(data) > maxManifestBytes {
		return nil, Manifest{}, errors.New("portable backup: manifest exceeds its size ceiling")
	}
	manifest, err := publicManifest(disk)
	return data, manifest, err
}

func parseManifest(data []byte, h artifactHeader, portable []byte) (Manifest, error) {
	if len(data) == 0 || len(data) > maxManifestBytes || len(data) != int(h.ManifestLen) {
		return Manifest{}, errors.New("portable backup: manifest size is invalid")
	}
	var disk diskManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&disk); err != nil {
		return Manifest{}, fmt.Errorf("portable backup: invalid manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("portable backup: manifest contains trailing data")
	}
	canonical, err := json.Marshal(disk)
	if err != nil || !bytes.Equal(canonical, data) {
		return Manifest{}, errors.New("portable backup: manifest is not canonical JSON")
	}
	if disk.Format != formatName || disk.Version != formatVersion ||
		disk.Database.Name != databaseMemberName || disk.Database.Size != h.DatabaseLen ||
		disk.PortableKey.Name != portableKeyMemberName || disk.PortableKey.Size != uint64(len(portable)) ||
		!validSHA256(disk.Database.SHA256) || !validSHA256(disk.PortableKey.SHA256) {
		return Manifest{}, errors.New("portable backup: manifest members are invalid")
	}
	if subtle.ConstantTimeCompare([]byte(disk.PortableKey.SHA256), []byte(sha256Hex(portable))) != 1 {
		return Manifest{}, ErrAuthentication
	}
	manifest, err := publicManifest(disk)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := validateMetadata(Metadata{
		ControllerVersion: manifest.ControllerVersion,
		SchemaVersion:     manifest.SchemaVersion,
		CreatedAt:         manifest.CreatedAt,
	}); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func publicManifest(disk diskManifest) (Manifest, error) {
	created, err := time.Parse(time.RFC3339Nano, disk.CreatedAt)
	if err != nil || created.Location() != time.UTC || created.Format(time.RFC3339Nano) != disk.CreatedAt {
		return Manifest{}, errors.New("portable backup: manifest creation time is not canonical UTC")
	}
	return Manifest{
		Format:            disk.Format,
		Version:           disk.Version,
		CreatedAt:         created,
		ControllerVersion: disk.ControllerVersion,
		SchemaVersion:     disk.SchemaVersion,
		Database:          disk.Database,
		PortableKey:       disk.PortableKey,
	}, nil
}

func deriveContentKey(keeper *secrets.Keeper, prefix, portable []byte) ([]byte, [32]byte, error) {
	digest := sha256.New()
	_, _ = digest.Write(prefix)
	_, _ = digest.Write(portable)
	var headerDigest [32]byte
	copy(headerDigest[:], digest.Sum(nil))
	message := make([]byte, 0, 32+len(headerDigest))
	message = append(message, prefix[48:80]...)
	message = append(message, headerDigest[:]...)
	key, err := keeper.HMACSHA256(contentKeyDomain, message)
	clear(message)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("portable backup: derive content key: %w", err)
	}
	return key, headerDigest, nil
}

func headerAAD(prefix, portable []byte) []byte {
	aad := make([]byte, 0, len(headerAADDomain)+len(prefix)+len(portable))
	aad = append(aad, headerAADDomain...)
	aad = append(aad, prefix...)
	aad = append(aad, portable...)
	return aad
}

func recordAAD(headerDigest [32]byte, kind byte, index uint32, plainSize uint64) []byte {
	aad := make([]byte, len(recordAADDomain)+32+1+4+8)
	offset := copy(aad, recordAADDomain)
	offset += copy(aad[offset:], headerDigest[:])
	aad[offset] = kind
	offset++
	binary.BigEndian.PutUint32(aad[offset:offset+4], index)
	binary.BigEndian.PutUint64(aad[offset+4:offset+12], plainSize)
	return aad
}

func recordNonce(seed [16]byte, sequence uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	copy(nonce, seed[:])
	binary.BigEndian.PutUint64(nonce[16:], sequence)
	return nonce
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func allZero(data []byte) bool {
	var combined byte
	for _, b := range data {
		combined |= b
	}
	return combined == 0
}
