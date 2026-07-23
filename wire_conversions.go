package agentsdk

import "github.com/airlockrun/agentsdk/wire"

func toWireAccess(access Access) wire.Access {
	return wire.Access(access)
}

func toWireAccesses(access []Access) []wire.Access {
	out := make([]wire.Access, len(access))
	for i, value := range access {
		out[i] = toWireAccess(value)
	}
	return out
}

func toWireAuthInjection(in AuthInjection) wire.AuthInjection {
	return wire.AuthInjection{Type: wire.AuthInjectionType(in.Type), Name: in.Name}
}

func toWireDisplayParts(parts []DisplayPart) []wire.DisplayPart {
	out := make([]wire.DisplayPart, len(parts))
	for i, part := range parts {
		out[i] = wire.DisplayPart{
			Type:     string(part.Type),
			Text:     part.Text,
			Source:   part.Source,
			URL:      part.URL,
			Data:     part.Data,
			Filename: part.Filename,
			MimeType: part.MimeType,
			Alt:      part.Alt,
			Duration: part.Duration,
		}
	}
	return out
}

func fileInfoFromWire(info wire.FileInfo) FileInfo {
	return FileInfo{
		Path:         FilePath(info.Path),
		Filename:     info.Filename,
		ContentType:  info.ContentType,
		Size:         info.Size,
		LastModified: info.LastModified,
	}
}

func mcpToolCallResponseFromWire(resp wire.MCPToolCallResponse) MCPToolCallResponse {
	content := make([]MCPContent, len(resp.Content))
	for i, item := range resp.Content {
		content[i] = MCPContent{
			Type:     item.Type,
			Text:     item.Text,
			URI:      item.URI,
			Name:     item.Name,
			MimeType: item.MimeType,
			Data:     item.Data,
		}
	}
	return MCPToolCallResponse{Content: content, IsError: resp.IsError}
}
