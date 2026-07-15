package agentsdk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestPublicAPISurface(t *testing.T) {
	want := strings.Fields(`
		const.AccessAdmin
		const.AccessPublic
		const.AccessUser
		const.AuthInjectAPIKey
		const.AuthInjectBearer
		const.AuthInjectPathPrefix
		const.AuthInjectQueryParam
		const.CapEmbedding
		const.CapImage
		const.CapSearch
		const.CapSpeech
		const.CapText
		const.CapTranscription
		const.CapVision
		const.ConnectionAuthNone
		const.ConnectionAuthOAuth
		const.ConnectionAuthToken
		const.HTMXVersion
		const.MCPAuthNone
		const.MCPAuthOAuth
		const.MCPAuthOAuthDiscovery
		const.MCPAuthToken
		const.MaxBufferedResponseBytes
		const.OpList
		const.OpRead
		const.OpWrite
		const.ScopeConv
		const.ScopeNone
		const.ScopeRun
		const.ScopeUser
		const.Version
		func.AgentFromContext
		func.AgentFromMigrationContext
		func.AgentURLFromContext
		func.IsAuthRequired
		func.MigrationExternalStep
		func.New
		func.NewHTTPError
		func.RequestJSON
		func.ScheduleFromContext
		func.UserFromContext
		func.WithLLMHint
		method.Agent.AddInstruction
		method.Agent.AddSensitive
		method.Agent.AgentURL
		method.Agent.CancelSchedule
		method.Agent.CheckFileAccess
		method.Agent.Close
		method.Agent.CopyFile
		method.Agent.DB
		method.Agent.DeleteFile
		method.Agent.Embed
		method.Agent.EmbeddingModel
		method.Agent.GenerateImage
		method.Agent.GenerateSpeech
		method.Agent.GenerateText
		method.Agent.Handler
		method.Agent.ImageModel
		method.Agent.LLM
		method.Agent.ListDir
		method.Agent.ListSchedules
		method.Agent.Logger
		method.Agent.MigrationContext
		method.Agent.MoveFile
		method.Agent.OpenFile
		method.Agent.OpenFileRange
		method.Agent.ReadFile
		method.Agent.ReadRange
		method.Agent.RegisterConnection
		method.Agent.RegisterCron
		method.Agent.RegisterDirectory
		method.Agent.RegisterEnvVar
		method.Agent.RegisterExecEndpoint
		method.Agent.RegisterMCP
		method.Agent.RegisterModel
		method.Agent.RegisterRoute
		method.Agent.RegisterSchedule
		method.Agent.RegisterStaticAsset
		method.Agent.RegisterTool
		method.Agent.RegisterTopic
		method.Agent.RegisterWebhook
		method.Agent.ScheduleAt
		method.Agent.Seal
		method.Agent.Serve
		method.Agent.ShareFileURL
		method.Agent.SpeechModel
		method.Agent.StatFile
		method.Agent.StreamText
		method.Agent.SyncDown
		method.Agent.SyncUp
		method.Agent.Transcribe
		method.Agent.TranscriptionModel
		method.Agent.Unseal
		method.Agent.WebSearch
		method.Agent.WriteFile
		method.AgentDB.ExecContext
		method.AgentDB.PingContext
		method.AgentDB.PrepareContext
		method.AgentDB.QueryContext
		method.AgentDB.QueryRowContext
		method.AgentDB.Transaction
		method.AuthRequiredError.Error
		method.ConnectionHandle.Request
		method.ConnectionHandle.RequestStream
		method.EnvVarHandle.Get
		method.EnvVarHandle.IsSecret
		method.EnvVarHandle.Refresh
		method.EnvVarHandle.Slug
		method.EventWriter.WriteError
		method.EventWriter.WriteEvent
		method.EventWriter.WriteProgress
		method.ExecError.Error
		method.ExecHandle.Run
		method.ExecHandle.RunStream
		method.ExecHandle.Slug
		method.HTTPError.Error
		method.HTTPError.Unwrap
		method.MCPHandle.CallTool
		method.TopicHandle.Publish
		method.TopicHandle.PublishToUser
		type.Access
		type.Agent
		type.AgentDB
		type.DBTX
		type.AuthInjection
		type.AuthInjectionType
		type.AuthRequiredError
		type.Config
		type.Connection
		type.ConnectionAuth
		type.ConnectionHandle
		type.ConnectionResponse
		type.Cron
		type.DirPath
		type.Directory
		type.DirectoryOpts
		type.DirectoryScope
		type.DisplayPart
		type.EnvVar
		type.EnvVarHandle
		type.EventWriter
		type.ExecCommand
		type.ExecEndpoint
		type.ExecError
		type.ExecExit
		type.ExecHandle
		type.ExecResult
		type.ExecStream
		type.FileInfo
		type.FileOp
		type.FilePath
		type.Instruction
		type.HTTPError
		type.ListOpts
		type.ListSchedulesFilter
		type.MCP
		type.MCPAuth
		type.MCPContent
		type.MCPHandle
		type.MCPToolCallResponse
		type.ModelCapability
		type.ModelSlot
		type.RegisterOption
		type.RequestOpts
		type.Route
		type.RouteHandlerFunc
		type.Schedule
		type.ScheduleAtRequest
		type.ScheduleHandlerFunc
		type.ScheduledFire
		type.ScheduledFireRef
		type.ShareFileResponse
		type.StaticAsset
		type.Topic
		type.TopicHandle
		type.User
		type.Webhook
		type.WebhookHandlerFunc
		var.Assets
		var.ErrAgentURLUnavailable
		var.ErrInvalidPath
		var.ErrNotFound
		var.ErrOutputTooLarge
	`)
	sort.Strings(want)

	packages, err := parser.ParseDir(token.NewFileSet(), ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg := packages["agentsdk"]
	if pkg == nil {
		t.Fatal("agentsdk package not found")
	}

	var got []string
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if !decl.Name.IsExported() {
					continue
				}
				if decl.Recv == nil {
					got = append(got, "func."+decl.Name.Name)
					continue
				}
				receiver := receiverName(decl.Recv.List[0].Type)
				if ast.IsExported(receiver) {
					got = append(got, "method."+receiver+"."+decl.Name.Name)
				}
			case *ast.GenDecl:
				kind := decl.Tok.String()
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						if spec.Name.IsExported() {
							got = append(got, kind+"."+spec.Name.Name)
						}
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							if name.IsExported() {
								got = append(got, kind+"."+name.Name)
							}
						}
					}
				}
			}
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("public API changed\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
}

func receiverName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.StarExpr:
		return receiverName(expr.X)
	default:
		panic("unexpected receiver expression")
	}
}
