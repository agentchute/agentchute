package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"os"

	"github.com/agentchute/agentchute/internal/loop"
)

type turnEndBlockRecord struct {
	Session     string `json:"session"`
	Turn        string `json:"turn"`
	Fingerprint string `json:"fingerprint"`
}

func (r turnEndBlockRecord) valid() bool {
	return r.Session != "" && r.Turn != "" && r.Fingerprint != ""
}

func readTurnEndBlockRecord(cfg *loop.Config, agentID string) turnEndBlockRecord {
	var record turnEndBlockRecord
	_ = json.Unmarshal([]byte(readTurnEndLastBlock(cfg, agentID)), &record)
	return record
}

func (in turnEndHookInput) turn() string {
	if in.TurnID != "" {
		return "turn:" + in.TurnID
	}
	if in.TurnIDCamel != "" {
		return "turn:" + in.TurnIDCamel
	}
	// Claude supplies a transcript rather than a turn ID. A user message's
	// UUID survives assistant/tool continuations but changes at the next prompt,
	// even if self-check never ran. Tool results and hook-injected meta messages
	// are not new prompts. Missing evidence keeps the Stop gate blocking.
	if id := lastTranscriptUserID(in.TranscriptPath); id != "" {
		return "user:" + id
	}
	return ""
}

func lastTranscriptUserID(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	// Bound work independently of session length. If the last prompt is outside
	// this tail, decline the retry exception rather than guessing at a turn.
	const maxTail = 8 << 20
	start := info.Size() - maxTail
	if start < 0 {
		start = 0
	}
	scan := bufio.NewScanner(io.NewSectionReader(f, start, info.Size()-start))
	scan.Buffer(make([]byte, 64<<10), maxTail)
	if start > 0 {
		scan.Scan() // the tail can start in the middle of a JSON line
	}
	last := ""
	for scan.Scan() {
		var entry struct {
			Type    string `json:"type"`
			UUID    string `json:"uuid"`
			IsMeta  bool   `json:"isMeta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scan.Bytes(), &entry) != nil {
			return ""
		}
		if entry.Type != "user" || entry.IsMeta {
			continue
		}
		var text string
		if json.Unmarshal(entry.Message.Content, &text) == nil {
			last = entry.UUID
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(entry.Message.Content, &blocks) != nil {
			return ""
		}
		if len(blocks) == 0 {
			last = entry.UUID
		}
		for _, block := range blocks {
			if block.Type != "tool_result" {
				last = entry.UUID
				break
			}
		}
	}
	if scan.Err() != nil {
		return ""
	}
	return last
}
