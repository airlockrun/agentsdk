// Package csvexample declares an app-owned CSV task with a native artifact tool.
package csvexample

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/goai/tool"
)

const Filename = "converted.csv"

// Input supplies already structured table cells, preserving their exact strings.
type Input struct {
	Header []string   `json:"header"`
	Rows   [][]string `json:"rows"`
}

// Output identifies the artifact and the number of data rows, excluding headers.
type Output struct {
	Filename string `json:"filename"`
	Rows     int    `json:"rows"`
}

// NewApp registers a real hosted task with an explicitly provisioned app-local
// artifact directory. Models/resources are bound by Airlock, not the factory.
func NewApp(outputDir string) (*agentsdk.Agent, *agentsdk.AgentHandle[Input, Output]) {
	if outputDir == "" {
		panic("csvexample: output directory is required")
	}
	a := agentsdk.New(agentsdk.Config{Description: "Convert structured table rows to a CSV artifact"})
	a.RegisterModel(&agentsdk.ModelSlot{
		Slug: "conversion", Capability: agentsdk.CapText,
		Description: "Execute the table conversion task",
	})
	write := tool.Typed[Input, Output]("write_csv").
		Description("Write the supplied header and rows exactly as CSV. Returns the artifact filename and number of data rows. Preserve all cells, including quotes, commas, newlines and Unicode.").
		Execute(func(ctx context.Context, in Input) (Output, error) {
			if err := ctx.Err(); err != nil {
				return Output{}, err
			}
			caller := agentsdk.CallerFromContext(ctx)
			if caller.Access() != agentsdk.AccessAdmin {
				return Output{}, errors.New("csvexample: admitted task admin context is required")
			}
			output, err := WriteCSV(ctx, outputDir, in)
			if err != nil {
				return Output{}, err
			}
			a.Logger(ctx).Info("CSV artifact written")
			return output, nil
		}).Build()
	handle := agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[Input, Output]{
		Slug: "convert_rows", Description: "Create a CSV file from structured table cells",
		Instructions: "The input JSON contains a header and rows. Use run_js to call tools.write_csv with the exact input, preserving every cell. Return the tool result from the script. Then call submit with kind output and the filename and rows returned by write_csv. Do not submit before successfully writing the file. Do not use file or database helpers: the private tool writes the artifact.",
		ModelSlot:    "conversion", Tools: []tool.Tool{write},
		Budget:      &agentsdk.AgentBudget{Steps: 8, Timeout: 2 * time.Minute},
		MaxAttempts: 1, MaxConcurrency: 1,
	})
	return a, handle
}

// WriteCSV is deterministic domain work; it has no host/model dependency.
func WriteCSV(ctx context.Context, outputDir string, in Input) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	if len(in.Header) == 0 {
		return Output{}, errors.New("csvexample: header is required")
	}
	for i, row := range in.Rows {
		if len(row) != len(in.Header) {
			return Output{}, fmt.Errorf("csvexample: row %d has %d cells, want %d", i, len(row), len(in.Header))
		}
	}
	var data bytes.Buffer
	writer := csv.NewWriter(&data)
	if err := writer.Write(in.Header); err != nil {
		return Output{}, err
	}
	if err := writer.WriteAll(in.Rows); err != nil {
		return Output{}, err
	}
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	if err := os.WriteFile(filepath.Join(outputDir, Filename), data.Bytes(), 0600); err != nil {
		return Output{}, err
	}
	return Output{Filename: Filename, Rows: len(in.Rows)}, nil
}
