package scaffold

import "testing"

func TestGeneratedArtifactPolicy(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "views/index_templ.go", want: true},
		{path: "views/static/app.css", want: true},
		{path: "internal/db/models.go", want: true},
		{path: "internal/db/queries.sql.go", want: true},
		{path: "internal/db/doc.go", want: false},
		{path: "agent", want: true},
		{path: "agent.exe", want: true},
		{path: "cmd/agent", want: false},
		{path: "styles/app.css", want: false},
		{path: "db/queries/queries.sql", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := IsGeneratedArtifact(tt.path); got != tt.want {
				t.Fatalf("IsGeneratedArtifact(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
