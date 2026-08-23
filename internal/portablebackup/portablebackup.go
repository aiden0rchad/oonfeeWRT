package portablebackup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"golang.org/x/crypto/chacha20poly1305"
)

// Result describes a completed, atomically published artifact.
type Result struct {
	Path     string
	Size     int64
	SHA256   string
	Manifest Manifest
}

type operations struct {
	afterCreateChunk   func()
	afterExtractChunk  func()
	afterExtractFinish func()
	afterLink          func() error
}

// Create encrypts an already-consistent database snapshot into outputPath.
// It makes no database, network, or router calls. outputPath's private parent
// must exist, and outputPath must not.
func Create(ctx context.Context, outputPath, databasePath string, live *secrets.Keeper,
	exportPassphrase []byte, meta Metadata) (Result, error) {
	return create(ctx, outputPath, databasePath, live, exportPassphrase, meta, operations{})
}

func create(ctx context.Context, outputPath, databasePath string, live *secrets.Keeper,
	exportPassphrase []byte, meta Metadata, ops operations) (_ Result, retErr error) {
	if ctx == nil {
		return Result{}, errors.New("portable backup: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if live == nil {
		return Result{}, errors.New("portable backup: secrets keeper is nil")
	}
	meta, err := validateMetadata(meta)
	if err != nil {
		return Result{}, err
	}
	absOutput, parentPath, outputName, outputRoot, err := openPrivateDestination(outputPath)
	if err != nil {
		return Result{}, err
	}
	defer outputRoot.Close()

	tempName := ""
	published := false
	defer func() {
		if tempName != "" {
			retErr = errors.Join(retErr, removeRootFile(outputRoot, tempName))
		}
		if retErr != nil && published {
			rollbackErr := removeRootFile(outputRoot, outputName)
			if rollbackErr == nil {
				rollbackErr = syncRoot(outputRoot, "output parent rollback")
			}
			retErr = errors.Join(retErr, rollbackErr)
		}
	}()

	database, databaseSize, err := openBoundedRegular(databasePath, "database snapshot", 1, MaxDatabaseBytes)
	if err != nil {
		return Result{}, err
	}
	defer database.Close()
	databaseDigest, err := hashExact(ctx, database, databaseSize)
	if err != nil {
		return Result{}, err
	}
	if _, err := database.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("portable backup: rewind database snapshot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	portable, err := live.ExportPortableKey(exportPassphrase)
	if err != nil {
		return Result{}, fmt.Errorf("portable backup: export portable key: %w", err)
	}
	defer clear(portable)
	if len(portable) == 0 || len(portable) > maxPortableKeyBytes {
		return Result{}, errors.New("portable backup: portable key exceeds its size ceiling")
	}
	manifestData, manifest, err := marshalManifest(meta, databaseSize, databaseDigest, portable)
	if err != nil {
		return Result{}, err
	}

	header := artifactHeader{
		PortableLen: uint32(len(portable)),
		ManifestLen: uint32(len(manifestData)),
		DatabaseLen: databaseSize,
		Chunks:      databaseChunks(databaseSize),
	}
	if err := randomNonzero(header.Salt[:]); err != nil {
		return Result{}, err
	}
	if err := randomNonzero(header.NonceSeed[:]); err != nil {
		return Result{}, err
	}
	prefix := encodeHeader(header)
	contentKey, headerDigest, err := deriveContentKey(live, prefix, portable)
	if err != nil {
		return Result{}, err
	}
	defer clear(contentKey)
	aead, err := chacha20poly1305.NewX(contentKey)
	if err != nil {
		return Result{}, fmt.Errorf("portable backup: initialize artifact cipher: %w", err)
	}

	temporary, name, err := createRandomFile(outputRoot, ".oonfeewrt-portable-", ".tmp")
	if err != nil {
		return Result{}, err
	}
	tempName = name
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			temporary.Close()
		}
	}()
	artifactHash := sha256.New()
	writer := &countingWriter{writer: io.MultiWriter(temporary, artifactHash)}
	if err := writeContext(ctx, writer, prefix); err != nil {
		return Result{}, err
	}
	if err := writeContext(ctx, writer, portable); err != nil {
		return Result{}, err
	}
	headerTag := aead.Seal(nil, recordNonce(header.NonceSeed, 0), nil, headerAAD(prefix, portable))
	if err := writeContext(ctx, writer, headerTag); err != nil {
		return Result{}, err
	}
	manifestCiphertext := aead.Seal(nil, recordNonce(header.NonceSeed, 1), manifestData,
		recordAAD(headerDigest, 'M', 0, uint64(len(manifestData))))
	if err := writeContext(ctx, writer, manifestCiphertext); err != nil {
		return Result{}, err
	}

	secondHash := sha256.New()
	buffer := make([]byte, chunkSize)
	defer clear(buffer)
	remaining := databaseSize
	for index := uint32(0); index < header.Chunks; index++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		plainSize := uint64(chunkSize)
		if remaining < plainSize {
			plainSize = remaining
		}
		plain := buffer[:int(plainSize)]
		if _, err := io.ReadFull(database, plain); err != nil {
			return Result{}, fmt.Errorf("portable backup: database snapshot changed while reading: %w", err)
		}
		_, _ = secondHash.Write(plain)
		ciphertext := aead.Seal(nil, recordNonce(header.NonceSeed, uint64(index)+2), plain,
			recordAAD(headerDigest, 'D', index, plainSize))
		if err := writeContext(ctx, writer, ciphertext); err != nil {
			clear(ciphertext)
			return Result{}, err
		}
		clear(ciphertext)
		clear(plain)
		remaining -= plainSize
		if ops.afterCreateChunk != nil {
			ops.afterCreateChunk()
		}
	}
	if err := requireEOF(database, "database snapshot"); err != nil {
		return Result{}, err
	}
	if subtle.ConstantTimeCompare(secondHash.Sum(nil), databaseDigest[:]) != 1 {
		return Result{}, errors.New("portable backup: database snapshot changed during creation")
	}
	if uint64(writer.count) != artifactSize(header) {
		return Result{}, errors.New("portable backup: internal artifact length mismatch")
	}
	if err := temporary.Chmod(0o600); err != nil {
		return Result{}, fmt.Errorf("portable backup: protect temporary output: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return Result{}, fmt.Errorf("portable backup: sync temporary output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Result{}, fmt.Errorf("portable backup: close temporary output: %w", err)
	}
	temporaryOpen = false
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := checkRootIdentity(outputRoot, parentPath); err != nil {
		return Result{}, err
	}
	if err := outputRoot.Link(tempName, outputName); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Result{}, fmt.Errorf("portable backup: output already exists: %w", os.ErrExist)
		}
		return Result{}, fmt.Errorf("portable backup: publish without overwrite: %w", err)
	}
	published = true
	if ops.afterLink != nil {
		if err := ops.afterLink(); err != nil {
			return Result{}, fmt.Errorf("portable backup: finish publication: %w", err)
		}
	}
	if err := syncRoot(outputRoot, "output parent"); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := removeRootFile(outputRoot, tempName); err != nil {
		return Result{}, err
	}
	tempName = ""
	if err := syncRoot(outputRoot, "output parent"); err != nil {
		return Result{}, err
	}
	if err := checkRootIdentity(outputRoot, parentPath); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{
		Path: absOutput, Size: writer.count,
		SHA256: hex.EncodeToString(artifactHash.Sum(nil)), Manifest: manifest,
	}, nil
}

// Extract authenticates an artifact into a new private staging directory.
// It never modifies live controller state. The caller must call Stage.Cleanup.
func Extract(ctx context.Context, artifactPath, stagingParent string,
	exportPassphrase []byte) (*Stage, error) {
	return extract(ctx, artifactPath, stagingParent, exportPassphrase, operations{})
}

func extract(ctx context.Context, artifactPath, stagingParent string,
	exportPassphrase []byte, ops operations) (_ *Stage, retErr error) {
	if ctx == nil {
		return nil, errors.New("portable backup: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	artifact, actualSize, err := openBoundedRegular(artifactPath, "artifact",
		headerSize+1+3*headerTagSize, maxArtifactBytes)
	if err != nil {
		return nil, err
	}
	defer artifact.Close()
	prefix := make([]byte, headerSize)
	if _, err := io.ReadFull(artifact, prefix); err != nil {
		return nil, errors.New("portable backup: artifact header is truncated")
	}
	header, err := decodeHeader(prefix)
	if err != nil {
		return nil, err
	}
	if artifactSize(header) != actualSize {
		return nil, errors.New("portable backup: artifact length does not match its header")
	}
	portable := make([]byte, header.PortableLen)
	defer clear(portable)
	if _, err := io.ReadFull(artifact, portable); err != nil {
		return nil, errors.New("portable backup: portable key is truncated")
	}
	temporaryKeeper, err := secrets.OpenPortableKey(portable, exportPassphrase)
	if err != nil {
		return nil, fmt.Errorf("portable backup: open portable key: %w", err)
	}
	contentKey, headerDigest, err := deriveContentKey(temporaryKeeper, prefix, portable)
	temporaryKeeper.Close()
	if err != nil {
		return nil, err
	}
	defer clear(contentKey)
	aead, err := chacha20poly1305.NewX(contentKey)
	if err != nil {
		return nil, fmt.Errorf("portable backup: initialize artifact cipher: %w", err)
	}
	headerTag := make([]byte, headerTagSize)
	if _, err := io.ReadFull(artifact, headerTag); err != nil {
		return nil, errors.New("portable backup: header authenticator is truncated")
	}
	plain, err := aead.Open(nil, recordNonce(header.NonceSeed, 0), headerTag,
		headerAAD(prefix, portable))
	if err != nil || len(plain) != 0 {
		clear(plain)
		return nil, ErrAuthentication
	}
	manifestCiphertext := make([]byte, int(header.ManifestLen)+headerTagSize)
	if _, err := io.ReadFull(artifact, manifestCiphertext); err != nil {
		return nil, errors.New("portable backup: encrypted manifest is truncated")
	}
	manifestData, err := aead.Open(nil, recordNonce(header.NonceSeed, 1), manifestCiphertext,
		recordAAD(headerDigest, 'M', 0, uint64(header.ManifestLen)))
	clear(manifestCiphertext)
	if err != nil {
		return nil, ErrAuthentication
	}
	defer clear(manifestData)
	manifest, err := parseManifest(manifestData, header, portable)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	builder, err := createStage(stagingParent, manifest, uint64(len(manifestData)))
	if err != nil {
		return nil, err
	}
	defer func() {
		if !builder.done {
			retErr = errors.Join(retErr, builder.abort())
		}
	}()
	database, err := builder.root.OpenFile(databaseMemberName,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("portable backup: create staged database: %w", err)
	}
	databaseOpen := true
	defer func() {
		if databaseOpen {
			database.Close()
		}
	}()
	if err := database.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("portable backup: protect staged database: %w", err)
	}

	databaseHash := sha256.New()
	remaining := header.DatabaseLen
	for index := uint32(0); index < header.Chunks; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		plainSize := uint64(chunkSize)
		if remaining < plainSize {
			plainSize = remaining
		}
		ciphertext := make([]byte, int(plainSize)+headerTagSize)
		if _, err := io.ReadFull(artifact, ciphertext); err != nil {
			clear(ciphertext)
			return nil, errors.New("portable backup: encrypted database is truncated")
		}
		plain, err := aead.Open(nil, recordNonce(header.NonceSeed, uint64(index)+2), ciphertext,
			recordAAD(headerDigest, 'D', index, plainSize))
		clear(ciphertext)
		if err != nil || len(plain) != int(plainSize) {
			clear(plain)
			return nil, ErrAuthentication
		}
		_, _ = databaseHash.Write(plain)
		if err := writeAll(database, plain); err != nil {
			clear(plain)
			return nil, fmt.Errorf("portable backup: write staged database: %w", err)
		}
		clear(plain)
		remaining -= plainSize
		if ops.afterExtractChunk != nil {
			ops.afterExtractChunk()
		}
	}
	if err := requireEOF(artifact, "artifact"); err != nil {
		return nil, err
	}
	expectedDatabaseHash, _ := hex.DecodeString(manifest.Database.SHA256)
	if subtle.ConstantTimeCompare(databaseHash.Sum(nil), expectedDatabaseHash) != 1 {
		return nil, ErrAuthentication
	}
	if err := database.Sync(); err != nil {
		return nil, fmt.Errorf("portable backup: sync staged database: %w", err)
	}
	if err := database.Close(); err != nil {
		return nil, fmt.Errorf("portable backup: close staged database: %w", err)
	}
	databaseOpen = false
	if err := writeStageFile(builder.root, portableKeyMemberName, portable); err != nil {
		return nil, err
	}
	if err := writeStageFile(builder.root, manifestMemberName, manifestData); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stage, err := builder.finish()
	if err != nil {
		return nil, err
	}
	if ops.afterExtractFinish != nil {
		ops.afterExtractFinish()
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, stage.Cleanup())
	}
	return stage, nil
}

func hashExact(ctx context.Context, file *os.File, size uint64) ([32]byte, error) {
	hasher := sha256.New()
	buffer := make([]byte, 128<<10)
	defer clear(buffer)
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		readSize := uint64(len(buffer))
		if remaining < readSize {
			readSize = remaining
		}
		if _, err := io.ReadFull(file, buffer[:int(readSize)]); err != nil {
			return [32]byte{}, fmt.Errorf("portable backup: database snapshot changed while hashing: %w", err)
		}
		_, _ = hasher.Write(buffer[:int(readSize)])
		clear(buffer[:int(readSize)])
		remaining -= readSize
	}
	if err := requireEOF(file, "database snapshot"); err != nil {
		return [32]byte{}, err
	}
	var result [32]byte
	copy(result[:], hasher.Sum(nil))
	return result, nil
}

func requireEOF(reader io.Reader, label string) error {
	var extra [1]byte
	n, err := reader.Read(extra[:])
	if n != 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("portable backup: %s has trailing or changing data", label)
	}
	return nil
}

func randomNonzero(destination []byte) error {
	for range 4 {
		if _, err := rand.Read(destination); err != nil {
			return fmt.Errorf("portable backup: generate artifact randomness: %w", err)
		}
		if !allZero(destination) {
			return nil
		}
	}
	return errors.New("portable backup: random generator returned an invalid all-zero value")
}

func removeRootFile(root *os.Root, name string) error {
	err := root.Remove(name)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("portable backup: remove %s: %w", name, err)
}

func writeContext(ctx context.Context, writer io.Writer, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeAll(writer, data); err != nil {
		return fmt.Errorf("portable backup: write artifact: %w", err)
	}
	return nil
}

type countingWriter struct {
	writer io.Writer
	count  int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.count += int64(n)
	return n, err
}
