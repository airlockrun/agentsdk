package wire

import (
	"fmt"

	"github.com/airlockrun/agentsdk/internal/binding"
)

// AgentCapability is a canonical capability ID, never a JS or direct-tool alias.
type AgentCapability string

// AgentCapabilityPolicy restricts generic operations. Private tools and bound
// MCPs are selected by the definition's own declarations, not by this list.
// An absent policy preserves the unrestricted generic inventory. An explicit
// policy with no entries permits no generic operations.
type AgentCapabilityPolicy struct {
	Allow []AgentCapability `json:"allow"`
}

const (
	CapabilityFileRead           AgentCapability = "air//file_read"
	CapabilityFileReadBytes      AgentCapability = "air//file_read_bytes"
	CapabilityFileReadRangeBytes AgentCapability = "air//file_read_range_bytes"
	CapabilityFileGrep           AgentCapability = "air//file_grep"
	CapabilityFileHead           AgentCapability = "air//file_head"
	CapabilityFileTail           AgentCapability = "air//file_tail"
	CapabilityFileLines          AgentCapability = "air//file_lines"
	CapabilityFileStat           AgentCapability = "air//file_stat"
	CapabilityFileExists         AgentCapability = "air//file_exists"
	CapabilityFileWrite          AgentCapability = "air//file_write"
	CapabilityFileDelete         AgentCapability = "air//file_delete"
	CapabilityFileList           AgentCapability = "air//file_list"
	CapabilityFileEncode         AgentCapability = "air//file_encode"
	CapabilityFileDecode         AgentCapability = "air//file_decode"
	CapabilityFileDecodeText     AgentCapability = "air//file_decode_text"
	CapabilityFileEditLines      AgentCapability = "air//file_edit_lines"
	CapabilityFileSed            AgentCapability = "air//file_sed"
	CapabilityQueryDB            AgentCapability = "air//query_db"
	CapabilityOutput             AgentCapability = "air//output"
	CapabilityFileShareURL       AgentCapability = "air//file_share_url"
	CapabilityHTTPRequest        AgentCapability = "air//http_request"
	CapabilityWebSearch          AgentCapability = "air//web_search"
	CapabilityAttachToContext    AgentCapability = "air//attach_to_context"
	CapabilityAnalyzeImage       AgentCapability = "air//analyze_image"
	CapabilityTranscribeAudio    AgentCapability = "air//transcribe_audio"
	CapabilityGenerateImage      AgentCapability = "air//generate_image"
	CapabilitySpeak              AgentCapability = "air//speak"
	CapabilityEmbed              AgentCapability = "air//embed"
	CapabilityRequestUpgrade     AgentCapability = "air//request_upgrade"
)

func validateAgentCapabilities(m AgentManifest, p *AgentCapabilityPolicy) error {
	if p == nil {
		return nil
	}
	known := map[AgentCapability]bool{}
	for _, id := range []AgentCapability{
		CapabilityFileRead, CapabilityFileReadBytes, CapabilityFileReadRangeBytes,
		CapabilityFileGrep, CapabilityFileHead, CapabilityFileTail, CapabilityFileLines,
		CapabilityFileStat, CapabilityFileExists, CapabilityFileWrite, CapabilityFileDelete,
		CapabilityFileList, CapabilityFileEncode, CapabilityFileDecode, CapabilityFileDecodeText,
		CapabilityFileEditLines, CapabilityFileSed, CapabilityQueryDB, CapabilityOutput,
		CapabilityFileShareURL, CapabilityHTTPRequest, CapabilityWebSearch, CapabilityAttachToContext,
		CapabilityAnalyzeImage, CapabilityTranscribeAudio, CapabilityGenerateImage, CapabilitySpeak,
		CapabilityEmbed, CapabilityRequestUpgrade,
	} {
		known[id] = true
	}
	for _, c := range m.Connections {
		for _, op := range []string{"request", "request_json"} {
			known[AgentCapability(binding.Local(binding.Connection, c.Slug, op).ID())] = true
		}
	}
	for _, topic := range m.Topics {
		for _, op := range []string{"subscribe", "unsubscribe"} {
			known[AgentCapability(binding.Local(binding.Topic, topic.Slug, op).ID())] = true
		}
	}
	seen := map[AgentCapability]bool{}
	for _, id := range p.Allow {
		if !known[id] || seen[id] {
			return fmt.Errorf("unknown or duplicate allowed capability %q", id)
		}
		seen[id] = true
	}
	// Attachment adapters resolve authorized metadata through the ordinary stat
	// capability. Require its explicit selection; never expand a policy in a helper.
	if seen[CapabilityAttachToContext] && !seen[CapabilityFileStat] {
		return fmt.Errorf("%s requires explicit %s", CapabilityAttachToContext, CapabilityFileStat)
	}
	return nil
}

func validateChildCapabilities(parent, child AgentDefinition) error {
	if parent.Capabilities == nil {
		return nil
	}
	if child.Capabilities == nil {
		return fmt.Errorf("restricted parent requires an explicit child capability policy: %s", child.Slug)
	}
	allowed := map[AgentCapability]bool{}
	for _, id := range parent.Capabilities.Allow {
		allowed[id] = true
	}
	for _, id := range child.Capabilities.Allow {
		if !allowed[id] {
			return fmt.Errorf("child %s exceeds parent capability policy: %s", child.Slug, id)
		}
	}
	bound := map[string]bool{}
	for _, slug := range parent.MCPs {
		bound[slug] = true
	}
	for _, slug := range child.MCPs {
		if !bound[slug] {
			return fmt.Errorf("child %s exceeds parent MCP selection: %s", child.Slug, slug)
		}
	}
	return nil
}
