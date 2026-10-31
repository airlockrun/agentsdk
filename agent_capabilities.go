package agentsdk

import "github.com/airlockrun/agentsdk/wire"

// AgentCapability identifies a generic operation by its canonical capability ID.
type AgentCapability = wire.AgentCapability

// AgentCapabilityPolicy is an explicit operation allowlist. Nil preserves the
// generic inventory; &AgentCapabilityPolicy{} permits no generic operations.
// Tools and MCPs remain controlled by AgentDefinition.Tools and .MCPs.
type AgentCapabilityPolicy = wire.AgentCapabilityPolicy

const (
	CapabilityFileRead           = wire.CapabilityFileRead
	CapabilityFileReadBytes      = wire.CapabilityFileReadBytes
	CapabilityFileReadRangeBytes = wire.CapabilityFileReadRangeBytes
	CapabilityFileGrep           = wire.CapabilityFileGrep
	CapabilityFileHead           = wire.CapabilityFileHead
	CapabilityFileTail           = wire.CapabilityFileTail
	CapabilityFileLines          = wire.CapabilityFileLines
	CapabilityFileStat           = wire.CapabilityFileStat
	CapabilityFileExists         = wire.CapabilityFileExists
	CapabilityFileWrite          = wire.CapabilityFileWrite
	CapabilityFileDelete         = wire.CapabilityFileDelete
	CapabilityFileList           = wire.CapabilityFileList
	CapabilityFileEncode         = wire.CapabilityFileEncode
	CapabilityFileDecode         = wire.CapabilityFileDecode
	CapabilityFileDecodeText     = wire.CapabilityFileDecodeText
	CapabilityFileEditLines      = wire.CapabilityFileEditLines
	CapabilityFileSed            = wire.CapabilityFileSed
	CapabilityOutput             = wire.CapabilityOutput
	CapabilityFileShareURL       = wire.CapabilityFileShareURL
	CapabilityHTTPRequest        = wire.CapabilityHTTPRequest
	CapabilityWebSearch          = wire.CapabilityWebSearch
	CapabilityAttachToContext    = wire.CapabilityAttachToContext
	CapabilityAnalyzeImage       = wire.CapabilityAnalyzeImage
	CapabilityTranscribeAudio    = wire.CapabilityTranscribeAudio
	CapabilityGenerateImage      = wire.CapabilityGenerateImage
	CapabilitySpeak              = wire.CapabilitySpeak
	CapabilityEmbed              = wire.CapabilityEmbed
	CapabilityRequestUpgrade     = wire.CapabilityRequestUpgrade
)
