//go:build darwin

package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const darwinServiceWaitTimeout = 10 * time.Second

type darwinService struct {
	kind, stateDir string
	mode           ServiceMode
	ops            Operations
	launchAgents   string
	rollbackHook   func(string) error
}

func newServiceManager(kind, stateDir string, mode ServiceMode, operations Operations, _ []string) serviceManager {
	launchAgents := ""
	if mode == ServiceUser {
		home, err := os.UserHomeDir()
		if err != nil {
			panic("connector: resolve macOS home directory: " + err.Error())
		}
		launchAgents = filepath.Join(home, "Library", "LaunchAgents")
	}
	return &darwinService{
		kind: kind, stateDir: stateDir, mode: mode, ops: operations,
		launchAgents: launchAgents,
	}
}

func (s *darwinService) requireUser() error {
	if s.mode != ServiceUser {
		return errors.New("connector: macOS system services are unsupported; use connector.ServiceUser")
	}
	return nil
}

func (s *darwinService) installationID() string {
	id := filepath.Base(s.stateDir)
	if validInstallationID(id) {
		return id
	}
	return ""
}

func (s *darwinService) label() string {
	label := "run.airlock.connector." + s.kind
	if id := s.installationID(); id != "" {
		label += "." + id
	}
	return label
}

func (s *darwinService) binary() string {
	return filepath.Join(s.stateDir, "bin", "airlock-connector-"+s.kind+"-"+s.installationID())
}

func (s *darwinService) plistPath() string {
	return filepath.Join(s.launchAgents, s.label()+".plist")
}

func (s *darwinService) domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func (s *darwinService) target() string { return s.domain() + "/" + s.label() }

func (s *darwinService) launchctl(ctx context.Context, args ...string) ([]byte, error) {
	return s.ops.Execute(ctx, "/bin/launchctl", args...)
}

type darwinLaunchState struct {
	registered bool
	running    bool
}

func launchdMissing(body []byte, err error) bool {
	message := strings.ToLower(string(body))
	if err != nil {
		message += " " + strings.ToLower(err.Error())
	}
	for _, marker := range []string{"could not find service", "service not found", "no such process", "no such file or directory", "113: could not find specified service"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func parseDarwinLaunchState(body []byte) darwinLaunchState {
	state := darwinLaunchState{registered: true}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && strings.TrimSpace(key) == "state" && strings.TrimSpace(value) == "running" {
			state.running = true
			break
		}
	}
	return state
}

func (s *darwinService) launchState(ctx context.Context) (darwinLaunchState, error) {
	body, err := s.launchctl(ctx, "print", s.target())
	if err == nil {
		return parseDarwinLaunchState(body), nil
	}
	if launchdMissing(body, err) {
		return darwinLaunchState{}, nil
	}
	return darwinLaunchState{}, err
}

func (s *darwinService) waitState(ctx context.Context, registered, running bool) error {
	waitCtx, cancel := context.WithTimeout(ctx, darwinServiceWaitTimeout)
	defer cancel()
	for {
		state, err := s.launchState(waitCtx)
		if err != nil {
			return err
		}
		if state.registered == registered && (!registered || state.running == running) {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("connector: launchd state transition timeout: %w", waitCtx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type launchdPlistConfig struct {
	label, binary, stateDir, installationID string
}

func launchdPlist(config launchdPlistConfig) ([]byte, error) {
	if strings.TrimSpace(config.label) == "" || !filepath.IsAbs(config.binary) || !filepath.IsAbs(config.stateDir) || !validInstallationID(config.installationID) {
		return nil, errors.New("connector: launchd plist requires a label, absolute paths, and an installation ID")
	}
	for _, value := range []string{config.label, config.binary, config.stateDir, config.installationID} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("connector: launchd plist values must be single-line strings without NUL bytes")
		}
	}
	var output bytes.Buffer
	output.WriteString(xml.Header)
	encoder := xml.NewEncoder(&output)
	encoder.Indent("", "  ")
	plist := xml.StartElement{Name: xml.Name{Local: "plist"}, Attr: []xml.Attr{{Name: xml.Name{Local: "version"}, Value: "1.0"}}}
	if err := encoder.EncodeToken(plist); err != nil {
		return nil, err
	}
	if err := encoder.EncodeToken(xml.StartElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return nil, err
	}
	writeString := func(key, value string) error {
		if err := encoder.EncodeElement(key, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
		return encoder.EncodeElement(value, xml.StartElement{Name: xml.Name{Local: "string"}})
	}
	if err := writeString("Label", config.label); err != nil {
		return nil, err
	}
	if err := encoder.EncodeElement("ProgramArguments", xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return nil, err
	}
	if err := encoder.EncodeToken(xml.StartElement{Name: xml.Name{Local: "array"}}); err != nil {
		return nil, err
	}
	for _, arg := range []string{config.binary, "run"} {
		if err := encoder.EncodeElement(arg, xml.StartElement{Name: xml.Name{Local: "string"}}); err != nil {
			return nil, err
		}
	}
	if err := encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: "array"}}); err != nil {
		return nil, err
	}
	for _, pair := range [][2]string{
		{"WorkingDirectory", config.stateDir},
		{"StandardOutPath", filepath.Join(config.stateDir, "connector.log")},
		{"StandardErrorPath", filepath.Join(config.stateDir, "connector.error.log")},
	} {
		if err := writeString(pair[0], pair[1]); err != nil {
			return nil, err
		}
	}
	if err := encoder.EncodeElement("EnvironmentVariables", xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return nil, err
	}
	if err := encoder.EncodeToken(xml.StartElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return nil, err
	}
	for _, pair := range [][2]string{
		{"AIRLOCK_CONNECTOR_INSTALLATION_ID", config.installationID},
		{"AIRLOCK_CONNECTOR_MODE", ""},
	} {
		if err := writeString(pair[0], pair[1]); err != nil {
			return nil, err
		}
	}
	if err := encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return nil, err
	}
	for _, key := range []string{"RunAtLoad", "KeepAlive"} {
		if err := encoder.EncodeElement(key, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return nil, err
		}
		if err := encoder.EncodeToken(xml.StartElement{Name: xml.Name{Local: "true"}}); err != nil {
			return nil, err
		}
		if err := encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: "true"}}); err != nil {
			return nil, err
		}
	}
	if err := encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return nil, err
	}
	if err := encoder.EncodeToken(plist.End()); err != nil {
		return nil, err
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	output.WriteByte('\n')
	if err := validateLaunchdPlist(output.Bytes()); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func validateLaunchdPlist(body []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root *launchdPlistNode
	var stack []*launchdPlistNode
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("connector: invalid launchd plist XML: %w", err)
		}
		switch value := token.(type) {
		case xml.Directive:
			return errors.New("connector: launchd plist contains an XML directive")
		case xml.ProcInst:
			if value.Target != "xml" || root != nil || len(stack) != 0 {
				return errors.New("connector: launchd plist contains an unsupported processing instruction")
			}
		case xml.StartElement:
			node := &launchdPlistNode{name: value.Name.Local}
			if len(stack) == 0 {
				if root != nil {
					return errors.New("connector: launchd plist contains multiple roots")
				}
				if value.Name.Local != "plist" || len(value.Attr) != 1 || value.Attr[0].Name.Local != "version" || value.Attr[0].Value != "1.0" {
					return errors.New("connector: launchd plist root must be plist version 1.0")
				}
				root = node
			} else {
				if len(value.Attr) != 0 {
					return errors.New("connector: launchd plist values cannot have XML attributes")
				}
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}
			stack = append(stack, node)
		case xml.CharData:
			if len(stack) == 0 {
				if strings.TrimSpace(string(value)) != "" {
					return errors.New("connector: launchd plist contains text outside its root")
				}
			} else {
				stack[len(stack)-1].text.Write(value)
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != value.Name.Local {
				return errors.New("connector: launchd plist has invalid element depth")
			}
			stack = stack[:len(stack)-1]
		}
	}
	if root == nil || len(stack) != 0 || len(root.children) != 1 || root.children[0].name != "dict" || strings.TrimSpace(root.text.String()) != "" {
		return errors.New("connector: launchd plist must contain one root dictionary")
	}
	values, err := launchdDictionary(root.children[0])
	if err != nil {
		return err
	}
	required := []string{"Label", "ProgramArguments", "WorkingDirectory", "StandardOutPath", "StandardErrorPath", "EnvironmentVariables", "RunAtLoad", "KeepAlive"}
	if len(values) != len(required) {
		return errors.New("connector: launchd plist has unexpected top-level keys")
	}
	for _, key := range required {
		if values[key] == nil {
			return fmt.Errorf("connector: launchd plist requires %s", key)
		}
	}
	label, err := launchdString(values["Label"])
	if err != nil || !strings.HasPrefix(label, "run.airlock.connector.") || strings.ContainsAny(label, "\x00\r\n") {
		return errors.New("connector: launchd plist has an invalid Label")
	}
	arguments := values["ProgramArguments"]
	if arguments.name != "array" || strings.TrimSpace(arguments.text.String()) != "" || len(arguments.children) != 2 {
		return errors.New("connector: launchd plist ProgramArguments must contain the executable and run command")
	}
	executable, executableErr := launchdString(arguments.children[0])
	command, commandErr := launchdString(arguments.children[1])
	if executableErr != nil || commandErr != nil || !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\x00\r\n") || command != "run" {
		return errors.New("connector: launchd plist ProgramArguments must use an absolute executable followed by run")
	}
	for _, key := range []string{"WorkingDirectory", "StandardOutPath", "StandardErrorPath"} {
		value, err := launchdString(values[key])
		if err != nil || !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("connector: launchd plist has an invalid %s", key)
		}
	}
	environment, err := launchdDictionary(values["EnvironmentVariables"])
	if err != nil || len(environment) != 2 {
		return errors.New("connector: launchd plist has invalid EnvironmentVariables")
	}
	installationID, idErr := launchdString(environment["AIRLOCK_CONNECTOR_INSTALLATION_ID"])
	mode, modeErr := launchdString(environment["AIRLOCK_CONNECTOR_MODE"])
	if idErr != nil || modeErr != nil || !validInstallationID(installationID) || mode != "" {
		return errors.New("connector: launchd plist must pin the installation ID and clear AIRLOCK_CONNECTOR_MODE")
	}
	for _, key := range []string{"RunAtLoad", "KeepAlive"} {
		value := values[key]
		if value.name != "true" || len(value.children) != 0 || strings.TrimSpace(value.text.String()) != "" {
			return fmt.Errorf("connector: launchd plist %s must be true", key)
		}
	}
	return nil
}

type launchdPlistNode struct {
	name     string
	text     bytes.Buffer
	children []*launchdPlistNode
}

func launchdDictionary(node *launchdPlistNode) (map[string]*launchdPlistNode, error) {
	if node == nil || node.name != "dict" || strings.TrimSpace(node.text.String()) != "" || len(node.children)%2 != 0 {
		return nil, errors.New("connector: launchd plist dictionary is malformed")
	}
	result := make(map[string]*launchdPlistNode, len(node.children)/2)
	for index := 0; index < len(node.children); index += 2 {
		keyNode := node.children[index]
		key := strings.TrimSpace(keyNode.text.String())
		if keyNode.name != "key" || len(keyNode.children) != 0 || key == "" {
			return nil, errors.New("connector: launchd plist dictionary key is malformed")
		}
		if result[key] != nil {
			return nil, fmt.Errorf("connector: launchd plist has duplicate key %q", key)
		}
		result[key] = node.children[index+1]
	}
	return result, nil
}

func launchdString(node *launchdPlistNode) (string, error) {
	if node == nil || node.name != "string" || len(node.children) != 0 {
		return "", errors.New("connector: launchd plist value must be a string")
	}
	return node.text.String(), nil
}

func (s *darwinService) plist() ([]byte, error) {
	return launchdPlist(launchdPlistConfig{
		label: s.label(), binary: s.binary(), stateDir: s.stateDir, installationID: s.installationID(),
	})
}

func (s *darwinService) PrepareIdentity(context.Context) error { return s.requireUser() }

func (s *darwinService) ValidateIdentity(context.Context, string, string) error {
	return errors.New("connector: macOS system services are unsupported; use connector.ServiceUser")
}

func (s *darwinService) Install(ctx context.Context) error {
	if err := s.requireUser(); err != nil {
		return err
	}
	if s.Installed() {
		return errors.New("connector: service is already installed; use upgrade")
	}
	state, err := s.launchState(ctx)
	if err != nil {
		return err
	}
	if state.registered {
		if _, err := s.launchctl(ctx, "bootout", s.target()); err != nil {
			return fmt.Errorf("connector: remove stale LaunchAgent registration: %w", err)
		}
		if err := s.waitState(ctx, false, false); err != nil {
			return err
		}
	}
	source, err := s.ops.Executable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	plist, err := s.plist()
	if err != nil {
		return err
	}
	if err := atomicWrite(s.binary(), binary, 0o755); err != nil {
		return err
	}
	return atomicWrite(s.plistPath(), plist, 0o600)
}

func (s *darwinService) Uninstall(ctx context.Context) error {
	if err := s.requireUser(); err != nil {
		return err
	}
	if err := s.Stop(ctx); err != nil {
		return err
	}
	for _, path := range []string{s.plistPath(), s.binary()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(s.rollbackRoot()); err != nil {
		return err
	}
	_, err := s.launchctl(ctx, "enable", s.target())
	return err
}

func (s *darwinService) Start(ctx context.Context) error {
	if err := s.requireUser(); err != nil {
		return err
	}
	if !s.Installed() {
		return errors.New("connector: macOS LaunchAgent is not installed")
	}
	state, err := s.launchState(ctx)
	if err != nil {
		return err
	}
	if state.running {
		return nil
	}
	if !state.registered {
		if _, err := s.launchctl(ctx, "bootstrap", s.domain(), s.plistPath()); err != nil {
			return err
		}
		state, err = s.launchState(ctx)
		if err != nil {
			return err
		}
	}
	if !state.running {
		if _, err := s.launchctl(ctx, "kickstart", s.target()); err != nil {
			return err
		}
	}
	return s.waitState(ctx, true, true)
}

func (s *darwinService) Stop(ctx context.Context) error {
	if err := s.requireUser(); err != nil {
		return err
	}
	state, err := s.launchState(ctx)
	if err != nil || !state.registered {
		return err
	}
	body, err := s.launchctl(ctx, "bootout", s.target())
	if err != nil && !launchdMissing(body, err) {
		return err
	}
	return s.waitState(ctx, false, false)
}

func (s *darwinService) Status(ctx context.Context) (string, error) {
	if err := s.requireUser(); err != nil {
		return "unsupported", err
	}
	state, err := s.launchState(ctx)
	if err != nil {
		return "unknown", err
	}
	if state.running {
		return "active", nil
	}
	return "inactive", nil
}

func (s *darwinService) Enable(ctx context.Context) error {
	if err := s.requireUser(); err != nil {
		return err
	}
	_, err := s.launchctl(ctx, "enable", s.target())
	return err
}

func (s *darwinService) Disable(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	_, err := s.launchctl(ctx, "disable", s.target())
	return err
}

func (s *darwinService) Reconfigure(context.Context) (func() error, error) {
	if err := s.requireUser(); err != nil {
		return nil, err
	}
	previous, err := os.ReadFile(s.plistPath())
	if err != nil {
		return nil, err
	}
	next, err := s.plist()
	if err != nil {
		return nil, err
	}
	restore := func() error { return atomicWrite(s.plistPath(), previous, 0o600) }
	if err := atomicWrite(s.plistPath(), next, 0o600); err != nil {
		return nil, err
	}
	return restore, nil
}

const darwinRollbackMetadataVersion = 1

type darwinRollbackMetadata struct {
	Version             int    `json:"version"`
	RollbackStateDigest string `json:"rollbackStateDigest"`
	BinaryDigest        string `json:"binaryDigest"`
	PlistDigest         string `json:"plistDigest"`
}

func digestBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func (s *darwinService) rollbackRoot() string { return s.binary() + ".rollback-sets" }

func (s *darwinService) rollbackStateDigest() (string, error) {
	body, err := os.ReadFile(filepath.Join(s.stateDir, "rollback-state.json"))
	if err != nil {
		return "", fmt.Errorf("connector: read generic rollback state generation: %w", err)
	}
	return digestBytes(body), nil
}

func (s *darwinService) retainRollbackSet(binary, plist []byte) (bool, error) {
	if err := validateLaunchdPlist(plist); err != nil {
		return false, err
	}
	stateDigest, err := s.rollbackStateDigest()
	if err != nil {
		return false, err
	}
	metadata := darwinRollbackMetadata{
		Version: darwinRollbackMetadataVersion, RollbackStateDigest: stateDigest,
		BinaryDigest: digestBytes(binary), PlistDigest: digestBytes(plist),
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return false, err
	}
	root := s.rollbackRoot()
	if err := ensurePrivateDirectory(root); err != nil {
		return false, err
	}
	pointerPath := filepath.Join(root, "current")
	current := ""
	if body, err := os.ReadFile(pointerPath); err == nil {
		current = strings.TrimSpace(string(body))
		if current != "a" && current != "b" {
			return false, errors.New("connector: invalid macOS rollback set pointer")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	next := "a"
	if current == "a" {
		next = "b"
	}
	stage, err := os.MkdirTemp(root, ".stage-")
	if err != nil {
		return false, err
	}
	defer func() {
		if stage != "" {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := atomicWrite(filepath.Join(stage, "binary"), binary, 0o700); err != nil {
		return false, err
	}
	if err := atomicWrite(filepath.Join(stage, "service.plist"), plist, 0o600); err != nil {
		return false, err
	}
	if err := atomicWrite(filepath.Join(stage, "metadata.json"), encodedMetadata, 0o600); err != nil {
		return false, err
	}
	if err := syncDirectory(stage); err != nil {
		return false, err
	}
	slot := filepath.Join(root, next)
	if err := os.RemoveAll(slot); err != nil {
		return false, err
	}
	if err := os.Rename(stage, slot); err != nil {
		return false, err
	}
	stage = ""
	if err := syncDirectory(root); err != nil {
		return false, err
	}
	if s.rollbackHook != nil {
		if err := s.rollbackHook("before-pointer"); err != nil {
			return false, err
		}
	}
	if err := atomicWrite(pointerPath, []byte(next+"\n"), 0o600); err != nil {
		pointer, readErr := os.ReadFile(pointerPath)
		committed := readErr == nil && strings.TrimSpace(string(pointer)) == next
		return committed, errors.Join(err, readErr)
	}
	if s.rollbackHook != nil {
		if err := s.rollbackHook("after-pointer"); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (s *darwinService) readRollbackSet() ([]byte, []byte, error) {
	root := s.rollbackRoot()
	pointer, err := os.ReadFile(filepath.Join(root, "current"))
	if err != nil {
		return nil, nil, err
	}
	slot := strings.TrimSpace(string(pointer))
	if slot != "a" && slot != "b" {
		return nil, nil, errors.New("connector: invalid macOS rollback set pointer")
	}
	binary, err := os.ReadFile(filepath.Join(root, slot, "binary"))
	if err != nil {
		return nil, nil, err
	}
	plist, err := os.ReadFile(filepath.Join(root, slot, "service.plist"))
	if err != nil {
		return nil, nil, err
	}
	if err := validateLaunchdPlist(plist); err != nil {
		return nil, nil, err
	}
	encodedMetadata, err := os.ReadFile(filepath.Join(root, slot, "metadata.json"))
	if err != nil {
		return nil, nil, err
	}
	var metadata darwinRollbackMetadata
	if err := strictUnmarshal(encodedMetadata, &metadata); err != nil {
		return nil, nil, fmt.Errorf("connector: decode macOS rollback metadata: %w", err)
	}
	stateDigest, err := s.rollbackStateDigest()
	if err != nil {
		return nil, nil, err
	}
	if metadata.Version != darwinRollbackMetadataVersion ||
		metadata.RollbackStateDigest != stateDigest ||
		metadata.BinaryDigest != digestBytes(binary) ||
		metadata.PlistDigest != digestBytes(plist) {
		return nil, nil, errors.New("connector: macOS rollback artifacts do not match the retained generic rollback state generation")
	}
	return binary, plist, nil
}

func (s *darwinService) Upgrade(ctx context.Context, activate func() error) (bool, error) {
	if err := s.requireUser(); err != nil {
		return false, err
	}
	source, err := s.ops.Executable()
	if err != nil {
		return false, err
	}
	next, err := os.ReadFile(source)
	if err != nil {
		return false, err
	}
	current, err := os.ReadFile(s.binary())
	if err != nil {
		return false, err
	}
	currentPlist, err := os.ReadFile(s.plistPath())
	if err != nil {
		return false, err
	}
	rollbackReady, err := s.retainRollbackSet(current, currentPlist)
	if err != nil {
		return rollbackReady, err
	}
	if err := s.Stop(ctx); err != nil {
		return true, err
	}
	if err := atomicWrite(s.binary(), next, 0o755); err != nil {
		return true, err
	}
	if err := activate(); err != nil {
		return true, err
	}
	if _, err := s.Reconfigure(ctx); err != nil {
		return true, err
	}
	if err := s.Start(ctx); err != nil {
		return true, err
	}
	return true, nil
}

func (s *darwinService) Installed() bool {
	_, binaryErr := os.Stat(s.binary())
	_, plistErr := os.Stat(s.plistPath())
	return binaryErr == nil && plistErr == nil
}

func (s *darwinService) Rollback(ctx context.Context) error {
	if err := s.requireUser(); err != nil {
		return err
	}
	binary, plist, err := s.readRollbackSet()
	if err != nil {
		return err
	}
	if err := atomicWrite(s.binary(), binary, 0o755); err != nil {
		return err
	}
	if err := atomicWrite(s.plistPath(), plist, 0o600); err != nil {
		return err
	}
	return s.Start(ctx)
}

func (s *darwinService) RollbackDigest() (string, error) {
	binary, _, err := s.readRollbackSet()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(binary)
	return hex.EncodeToString(digest[:]), nil
}
