package connector

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

const (
	defaultMaxReadBytes  = protocol.MaxInlineFileBytes
	defaultMaxWriteBytes = 64 << 30
	defaultListLimit     = 100
	maximumListLimit     = 1000
)

type LocalDirectoryOptions struct {
	MaxReadBytes  int64
	MaxWriteBytes int64
	HTTPClient    *http.Client
}

type LocalDirectoryProvider struct {
	path          string
	binding       *DirectorySetting
	rootMu        sync.Mutex
	root          *os.Root
	maxReadBytes  int64
	maxWriteBytes int64
	http          *http.Client
	origins       map[string]bool
}

// LocalDirectory opens a confined local filesystem root. The returned value
// must be registered with a Directory definition and is closed by Runtime.
func LocalDirectory(rootPath string, options ...LocalDirectoryOptions) *LocalDirectoryProvider {
	if rootPath == "" {
		panic("connector: LocalDirectory requires a static absolute root; use BoundLocalDirectory for a configured root")
	}
	return newLocalDirectory(rootPath, nil, options)
}

// BoundLocalDirectory opens a confined local filesystem root whose path is
// supplied by an explicitly selected directory setting at runtime.
func BoundLocalDirectory(setting DirectorySetting, options ...LocalDirectoryOptions) *LocalDirectoryProvider {
	if setting.settings == nil || setting.field.kind != "directory" {
		panic("connector: BoundLocalDirectory requires a directory setting")
	}
	return newLocalDirectory("", &setting, options)
}

func newLocalDirectory(rootPath string, binding *DirectorySetting, options []LocalDirectoryOptions) *LocalDirectoryProvider {
	if len(options) > 1 {
		panic("connector: local directory accepts at most one options value")
	}
	if rootPath != "" && !pathIsAbsolute(rootPath) {
		panic("connector: LocalDirectory root must be absolute")
	}
	config := LocalDirectoryOptions{}
	if len(options) == 1 {
		config = options[0]
	}
	if config.MaxReadBytes == 0 {
		config.MaxReadBytes = defaultMaxReadBytes
	}
	if config.MaxWriteBytes == 0 {
		config.MaxWriteBytes = defaultMaxWriteBytes
	}
	if config.MaxReadBytes < 1 || config.MaxWriteBytes < 1 {
		panic("connector: LocalDirectory limits must be positive")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 10 * time.Minute}
	}
	return &LocalDirectoryProvider{
		path: rootPath, binding: binding, maxReadBytes: config.MaxReadBytes,
		maxWriteBytes: config.MaxWriteBytes, http: config.HTTPClient, origins: make(map[string]bool),
	}
}

func (d *LocalDirectoryProvider) rebind(rootPath string) error {
	if rootPath != "" && !pathIsAbsolute(rootPath) {
		return errors.New("connector: local directory root must be absolute")
	}
	d.rootMu.Lock()
	defer d.rootMu.Unlock()
	if d.path == rootPath {
		return nil
	}
	if d.root != nil {
		if err := d.root.Close(); err != nil {
			return err
		}
		d.root = nil
	}
	d.path = rootPath
	return nil
}

func (d *LocalDirectoryProvider) Close() error {
	d.rootMu.Lock()
	defer d.rootMu.Unlock()
	if d.root == nil {
		return nil
	}
	err := d.root.Close()
	d.root = nil
	return err
}

func (d *LocalDirectoryProvider) configured() bool {
	d.rootMu.Lock()
	defer d.rootMu.Unlock()
	return d.path != ""
}

func (d *LocalDirectoryProvider) initialize() error {
	d.rootMu.Lock()
	defer d.rootMu.Unlock()
	if d.root != nil {
		return nil
	}
	if d.path == "" {
		return errors.New("connector: local directory path is not configured")
	}
	root, err := openLocalRoot(d.path)
	if err != nil {
		return fmt.Errorf("connector: open local directory: %w", err)
	}
	d.root = root
	return nil
}

func (d *LocalDirectoryProvider) setOrigins(values []string) {
	d.origins = make(map[string]bool, len(values))
	for _, value := range canonicalOrigins(values) {
		d.origins[value] = true
	}
}

func (d *LocalDirectoryProvider) Stat(relative string) (protocol.DirectoryEntry, error) {
	if err := d.initialize(); err != nil {
		return protocol.DirectoryEntry{}, err
	}
	name, err := cleanRelative(relative, true)
	if err != nil {
		return protocol.DirectoryEntry{}, err
	}
	info, err := d.root.Stat(name)
	if err != nil {
		return protocol.DirectoryEntry{}, err
	}
	return directoryEntry(relative, info), nil
}

func (d *LocalDirectoryProvider) List(relative, cursor string, limit int) (protocol.DirectoryListResponse, error) {
	return d.list(context.Background(), relative, cursor, limit)
}

func (d *LocalDirectoryProvider) list(ctx context.Context, relative, cursor string, limit int) (protocol.DirectoryListResponse, error) {
	if err := d.initialize(); err != nil {
		return protocol.DirectoryListResponse{}, err
	}
	name, err := cleanRelative(relative, true)
	if err != nil {
		return protocol.DirectoryListResponse{}, err
	}
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maximumListLimit {
		return protocol.DirectoryListResponse{}, fmt.Errorf("connector: list limit must be between 1 and %d", maximumListLimit)
	}
	file, err := d.root.Open(name)
	if err != nil {
		return protocol.DirectoryListResponse{}, err
	}
	defer file.Close()
	entries, err := file.ReadDir(protocol.MaxDirectoryScanEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return protocol.DirectoryListResponse{}, err
	}
	if len(entries) > protocol.MaxDirectoryScanEntries {
		return protocol.DirectoryListResponse{}, fmt.Errorf("connector: directory scan exceeds %d entries", protocol.MaxDirectoryScanEntries)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := protocol.DirectoryListResponse{Entries: make([]protocol.DirectoryEntry, 0, limit)}
	for index, entry := range entries {
		if err := ctx.Err(); err != nil {
			return protocol.DirectoryListResponse{}, err
		}
		if entry.Name() <= cursor {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return protocol.DirectoryListResponse{}, err
		}
		entryPath := entry.Name()
		if name != "." {
			entryPath = path.Join(name, entry.Name())
		}
		result.Entries = append(result.Entries, directoryEntry(entryPath, info))
		if len(result.Entries) == limit {
			if index < len(entries)-1 {
				result.NextCursor = entry.Name()
			}
			break
		}
	}
	return result, nil
}

func (d *LocalDirectoryProvider) Read(relative string, offset, length int64) (protocol.DirectoryReadResponse, error) {
	if err := d.initialize(); err != nil {
		return protocol.DirectoryReadResponse{}, err
	}
	name, err := cleanRelative(relative, false)
	if err != nil {
		return protocol.DirectoryReadResponse{}, err
	}
	if offset < 0 || length < 0 {
		return protocol.DirectoryReadResponse{}, errors.New("connector: read offset and length cannot be negative")
	}
	file, err := d.root.Open(name)
	if err != nil {
		return protocol.DirectoryReadResponse{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return protocol.DirectoryReadResponse{}, err
	}
	if !info.Mode().IsRegular() {
		return protocol.DirectoryReadResponse{}, errors.New("connector: read requires a regular file")
	}
	if offset > info.Size() {
		return protocol.DirectoryReadResponse{}, errors.New("connector: read offset exceeds file size")
	}
	remaining := info.Size() - offset
	if length == 0 || length > remaining {
		length = remaining
	}
	if length > d.maxReadBytes {
		return protocol.DirectoryReadResponse{}, fmt.Errorf("connector: read exceeds %d bytes", d.maxReadBytes)
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return protocol.DirectoryReadResponse{}, err
	}
	body, err := io.ReadAll(io.LimitReader(file, length))
	if err != nil {
		return protocol.DirectoryReadResponse{}, err
	}
	return protocol.DirectoryReadResponse{Entry: directoryEntry(name, info), Data: body}, nil
}

func (d *LocalDirectoryProvider) Write(relative string, body []byte, overwrite bool) (protocol.DirectoryEntry, error) {
	if err := d.initialize(); err != nil {
		return protocol.DirectoryEntry{}, err
	}
	if int64(len(body)) > d.maxWriteBytes {
		return protocol.DirectoryEntry{}, fmt.Errorf("connector: write exceeds %d bytes", d.maxWriteBytes)
	}
	return d.atomicWrite(relative, bytes.NewReader(body), int64(len(body)), int64(len(body)), "", overwrite)
}

func (d *LocalDirectoryProvider) Delete(relative string) error {
	if err := d.initialize(); err != nil {
		return err
	}
	name, err := cleanRelative(relative, false)
	if err != nil {
		return err
	}
	info, err := d.root.Lstat(name)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("connector: delete does not remove directories")
	}
	return d.root.Remove(name)
}

func (d *LocalDirectoryProvider) Move(from, to string, overwrite bool) error {
	if err := d.initialize(); err != nil {
		return err
	}
	source, err := cleanRelative(from, false)
	if err != nil {
		return err
	}
	destination, err := cleanRelative(to, false)
	if err != nil {
		return err
	}
	if err := d.root.MkdirAll(path.Dir(destination), 0o700); err != nil {
		return err
	}
	if !overwrite {
		info, err := d.root.Lstat(source)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("connector: non-overwriting move requires a regular file")
		}
		if err := d.root.Link(source, destination); err != nil {
			return err
		}
		if err := d.root.Remove(source); err != nil {
			_ = d.root.Remove(destination)
			return err
		}
		return nil
	}
	return d.root.Rename(source, destination)
}

func (d *LocalDirectoryProvider) Import(ctx context.Context, request protocol.DirectoryImportRequest) (protocol.DirectoryEntry, error) {
	if err := d.initialize(); err != nil {
		return protocol.DirectoryEntry{}, err
	}
	if err := d.validateGrant(request.Grant); err != nil {
		return protocol.DirectoryEntry{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.Grant.URL, nil)
	if err != nil {
		return protocol.DirectoryEntry{}, err
	}
	for name, value := range request.Grant.Headers {
		httpRequest.Header.Set(name, value)
	}
	response, err := d.transferClient().Do(httpRequest)
	if err != nil {
		return protocol.DirectoryEntry{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return protocol.DirectoryEntry{}, fmt.Errorf("connector: import returned HTTP %d", response.StatusCode)
	}
	maximum := request.Grant.MaximumSize
	if maximum == 0 || maximum > d.maxWriteBytes {
		maximum = d.maxWriteBytes
	}
	return d.atomicWrite(request.Path, io.LimitReader(response.Body, maximum+1), maximum, request.Grant.ExpectedSize, request.Grant.ExpectedSHA256, request.Overwrite)
}

func (d *LocalDirectoryProvider) Export(ctx context.Context, request protocol.DirectoryExportRequest) (protocol.DirectoryExportResponse, error) {
	if err := d.initialize(); err != nil {
		return protocol.DirectoryExportResponse{}, err
	}
	if err := d.validateGrant(request.Grant); err != nil {
		return protocol.DirectoryExportResponse{}, err
	}
	name, err := cleanRelative(request.Path, false)
	if err != nil {
		return protocol.DirectoryExportResponse{}, err
	}
	file, err := d.root.Open(name)
	if err != nil {
		return protocol.DirectoryExportResponse{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return protocol.DirectoryExportResponse{}, errors.New("connector: export requires a regular file")
	}
	if request.Grant.MaximumSize > 0 && info.Size() > request.Grant.MaximumSize {
		return protocol.DirectoryExportResponse{}, errors.New("connector: export exceeds grant maximum size")
	}
	if request.PartSize < 1 || len(request.Parts) == 0 {
		return protocol.DirectoryExportResponse{}, errors.New("connector: export requires part grants and a positive part size")
	}
	if request.PartSize > protocol.MaxTransferPartBytes {
		return protocol.DirectoryExportResponse{}, fmt.Errorf("connector: export part size exceeds %d bytes", protocol.MaxTransferPartBytes)
	}
	if len(request.Parts) > protocol.MaxTransferParts {
		return protocol.DirectoryExportResponse{}, fmt.Errorf("connector: export has more than %d parts", protocol.MaxTransferParts)
	}
	hasher := sha256.New()
	result := protocol.DirectoryExportResponse{Parts: make([]protocol.UploadedPart, 0, len(request.Parts)), Size: info.Size()}
	var offset int64
	seenParts := make(map[int]bool, len(request.Parts))
	for _, part := range request.Parts {
		if part.Number < 1 || seenParts[part.Number] {
			return protocol.DirectoryExportResponse{}, errors.New("connector: export part numbers must be positive and unique")
		}
		seenParts[part.Number] = true
		if err := d.validateURL(part.URL); err != nil {
			return protocol.DirectoryExportResponse{}, err
		}
		remaining := info.Size() - offset
		if remaining <= 0 {
			break
		}
		partSize := min(request.PartSize, remaining)
		body := io.TeeReader(io.NewSectionReader(file, offset, partSize), hasher)
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPut, part.URL, body)
		if err != nil {
			return protocol.DirectoryExportResponse{}, err
		}
		httpRequest.ContentLength = partSize
		for name, value := range request.Grant.Headers {
			httpRequest.Header.Set(name, value)
		}
		response, err := d.transferClient().Do(httpRequest)
		if err != nil {
			return protocol.DirectoryExportResponse{}, err
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return protocol.DirectoryExportResponse{}, errors.Join(readErr, closeErr)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return protocol.DirectoryExportResponse{}, fmt.Errorf("connector: export part %d returned HTTP %d: %s", part.Number, response.StatusCode, responseBody)
		}
		result.Parts = append(result.Parts, protocol.UploadedPart{Number: part.Number, ETag: response.Header.Get("ETag"), Size: partSize})
		offset += partSize
	}
	if uploaded := sumParts(result.Parts); uploaded != info.Size() {
		return protocol.DirectoryExportResponse{}, errors.New("connector: export grants did not cover the complete file")
	}
	result.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	if request.Grant.ExpectedSHA256 != "" && !strings.EqualFold(request.Grant.ExpectedSHA256, result.SHA256) {
		return protocol.DirectoryExportResponse{}, errors.New("connector: exported file checksum mismatch")
	}
	return result, nil
}

func (d *LocalDirectoryProvider) atomicWrite(relative string, reader io.Reader, maximum, expectedSize int64, expectedHash string, overwrite bool) (protocol.DirectoryEntry, error) {
	name, err := cleanRelative(relative, false)
	if err != nil {
		return protocol.DirectoryEntry{}, err
	}
	if err := d.root.MkdirAll(path.Dir(name), 0o700); err != nil {
		return protocol.DirectoryEntry{}, err
	}
	temporary := path.Join(path.Dir(name), ".airlock-"+randomSuffix())
	file, err := d.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return protocol.DirectoryEntry{}, err
	}
	defer d.root.Remove(temporary)
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(reader, maximum+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return protocol.DirectoryEntry{}, copyErr
	}
	if written > maximum {
		return protocol.DirectoryEntry{}, fmt.Errorf("connector: write exceeds %d bytes", maximum)
	}
	if expectedSize > 0 && written != expectedSize {
		return protocol.DirectoryEntry{}, errors.New("connector: imported file size mismatch")
	}
	if syncErr != nil {
		return protocol.DirectoryEntry{}, syncErr
	}
	if closeErr != nil {
		return protocol.DirectoryEntry{}, closeErr
	}
	if expectedHash != "" && !strings.EqualFold(expectedHash, hex.EncodeToString(hasher.Sum(nil))) {
		return protocol.DirectoryEntry{}, errors.New("connector: imported file checksum mismatch")
	}
	if overwrite {
		if err := d.root.Rename(temporary, name); err != nil {
			return protocol.DirectoryEntry{}, err
		}
	} else {
		if err := d.root.Link(temporary, name); err != nil {
			return protocol.DirectoryEntry{}, err
		}
		if err := d.root.Remove(temporary); err != nil {
			return protocol.DirectoryEntry{}, err
		}
	}
	return d.Stat(name)
}

func (d *LocalDirectoryProvider) validateGrant(grant protocol.TransferGrant) error {
	if !grant.ExpiresAt.After(time.Now()) {
		return errors.New("connector: transfer grant is expired")
	}
	if grant.MaximumSize < 1 || grant.ExpectedSize < 0 || (grant.ExpectedSize > 0 && grant.ExpectedSize > grant.MaximumSize) {
		return errors.New("connector: transfer grant has invalid size limits")
	}
	if grant.ExpectedSHA256 != "" {
		decoded, err := hex.DecodeString(grant.ExpectedSHA256)
		if err != nil || len(decoded) != sha256.Size || grant.ExpectedSHA256 != strings.ToLower(grant.ExpectedSHA256) {
			return errors.New("connector: transfer grant has an invalid SHA-256 checksum")
		}
	}
	if len(grant.Headers) > 64 {
		return errors.New("connector: transfer grant has too many headers")
	}
	for name, value := range grant.Headers {
		if len(name) > 256 || len(value) > 8192 || strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") {
			return fmt.Errorf("connector: transfer grant header %q is not allowed", name)
		}
	}
	return d.validateURL(grant.URL)
}

func (d *LocalDirectoryProvider) validateURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return errors.New("connector: transfer URL must be an absolute HTTPS URL without user information")
	}
	origin := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
	if !d.origins[origin] {
		return fmt.Errorf("connector: transfer origin %q was not approved by the host", origin)
	}
	return nil
}

func (d *LocalDirectoryProvider) transferClient() *http.Client {
	copy := *d.http
	previous := copy.CheckRedirect
	copy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("connector: stopped after 10 transfer redirects")
		}
		if err := d.validateURL(request.URL.String()); err != nil {
			return err
		}
		if len(via) > 0 && originOf(via[0].URL) != originOf(request.URL) {
			return errors.New("connector: cross-origin transfer redirect rejected")
		}
		if previous != nil {
			return previous(request, via)
		}
		return nil
	}
	return &copy
}

func originOf(value *url.URL) string {
	return strings.ToLower(value.Scheme) + "://" + strings.ToLower(value.Host)
}

func cleanRelative(value string, allowRoot bool) (string, error) {
	if value == "" && allowRoot {
		return ".", nil
	}
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") || path.IsAbs(value) {
		return "", errors.New("connector: path must be a non-empty canonical relative path")
	}
	clean := path.Clean(value)
	if clean != value || clean == ".." || strings.HasPrefix(clean, "../") || (!allowRoot && clean == ".") {
		return "", errors.New("connector: path must be a canonical relative path without traversal")
	}
	return clean, nil
}

func directoryEntry(name string, info os.FileInfo) protocol.DirectoryEntry {
	contentType := ""
	if !info.IsDir() {
		contentType = mime.TypeByExtension(path.Ext(info.Name()))
	}
	return protocol.DirectoryEntry{
		Path: name, Name: info.Name(), Size: info.Size(), Mode: uint32(info.Mode().Perm()),
		Directory: info.IsDir(), ContentType: contentType, ModifiedAt: info.ModTime().UTC(),
	}
}

func randomSuffix() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("connector: random temporary name: " + err.Error())
	}
	return hex.EncodeToString(value[:])
}

func sumParts(parts []protocol.UploadedPart) int64 {
	var total int64
	for _, part := range parts {
		total += part.Size
	}
	return total
}
