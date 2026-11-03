package ociui

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"testing"

	"github.com/ohseeeye/dreg/internal/explore/internal/soci"
)

func TestExplorerDebugLogging(t *testing.T) {
	previous := slog.Default()
	writer, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var output bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: level})))
			// Constructor-only: no archive content, cache, or upstream is opened.
			soci.FS(nil, nil, "test-prefix", "test-ref", 1024, "", nil)
			if level == slog.LevelInfo {
				if output.Len() != 0 {
					t.Fatalf("debug logging leaked at INFO: %s", output.String())
				}
				return
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["level"] != "DEBUG" || record["prefix"] != "test-prefix" || record["ref"] != "test-ref" {
				t.Fatalf("missing structured debug fields: %v", record)
			}
		})
	}
}
