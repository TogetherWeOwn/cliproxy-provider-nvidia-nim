package main

// Ported from TogetherWeOwn/cliproxy-provider-synthetic aggregate.go (same
// owner, MIT). Candidate for the shared provider kit once a second release of
// the kit is cut; kept local so this fix ships without a kit release.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const maxAggregateBytes = 32 << 20

func invalidSSE() error {
	return &pluginabi.Error{Code: "invalid_stream", Message: "NVIDIA NIM returned an invalid or incomplete completion stream", HTTPStatus: 502, Retryable: true}
}

// Read across arbitrary host chunk boundaries; no goroutine or unbounded pipe is
// needed to adapt the host's channel to the SSE decoder.
type chunkReader struct {
	ctx     context.Context
	chunks  <-chan pluginapi.ExecutorStreamChunk
	pending []byte
	err     error
	total   int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case c, ok := <-r.chunks:
			if !ok {
				return 0, io.EOF
			}
			r.total += len(c.Payload)
			if r.total > maxAggregateBytes {
				return 0, invalidSSE()
			}
			r.pending = c.Payload
			r.err = c.Err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

type streamChoice struct {
	Index   int            `json:"index"`
	Message map[string]any `json:"message"`
	Finish  any            `json:"finish_reason"`
	tools   map[int]map[string]any
}
type streamEvent struct {
	ID          string          `json:"id"`
	Model       string          `json:"model"`
	Created     int64           `json:"created"`
	Fingerprint string          `json:"system_fingerprint"`
	Error       json.RawMessage `json:"error"`
	Choices     []struct {
		Index  int            `json:"index"`
		Delta  map[string]any `json:"delta"`
		Finish any            `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

// Join fragments once at completion instead of repeatedly copying growing
// content/tool arguments on every token (quadratic for long replies).
type textFragments []string

func appendFields(dst, src map[string]any) {
	for k, v := range src {
		if s, ok := v.(string); ok && k != "role" && k != "id" && k != "type" {
			fragments, _ := dst[k].(textFragments)
			dst[k] = append(fragments, s)
		} else if v != nil {
			dst[k] = v
		}
	}
}
func finishFields(fields map[string]any) {
	for k, v := range fields {
		switch value := v.(type) {
		case textFragments:
			fields[k] = strings.Join(value, "")
		case map[string]any:
			finishFields(value)
		case []map[string]any:
			for _, item := range value {
				finishFields(item)
			}
		}
	}
}
func sumUsage(dst, src map[string]any) {
	for k, v := range src {
		switch n := v.(type) {
		case json.Number:
			value, err := n.Int64()
			if err != nil {
				continue
			}
			old, _ := dst[k].(int64)
			dst[k] = old + value
		case map[string]any:
			nested, _ := dst[k].(map[string]any)
			if nested == nil {
				nested = make(map[string]any)
				dst[k] = nested
			}
			sumUsage(nested, n)
		}
	}
}

// aggregateSSE constructs the OpenAI chat.completion shape, including indexed
// choices, fragmented tool arguments, reasoning and reported token usage. A
// missing terminal marker is an error, not a fabricated successful completion.
func aggregateSSE(ctx context.Context, chunks <-chan pluginapi.ExecutorStreamChunk) ([]byte, error) {
	scanner := bufio.NewScanner(&chunkReader{ctx: ctx, chunks: chunks})
	scanner.Buffer(make([]byte, 4096), 2<<20)
	choices := make(map[int]*streamChoice)
	usage := make(map[string]any)
	out := map[string]any{"object": "chat.completion"}
	var data []string
	done := false
	consume := func() error {
		if len(data) == 0 {
			return nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		if strings.TrimSpace(raw) == "[DONE]" {
			done = true
			return nil
		}
		var event streamEvent
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		if d.Decode(&event) != nil || len(event.Error) > 0 || (len(event.Choices) == 0 && len(event.Usage) == 0) {
			return invalidSSE()
		}
		var trailing any
		if d.Decode(&trailing) != io.EOF {
			return invalidSSE()
		}
		if event.ID != "" {
			out["id"] = event.ID
		}
		if event.Model != "" {
			out["model"] = event.Model
		}
		if event.Created != 0 {
			out["created"] = event.Created
		}
		if event.Fingerprint != "" {
			out["system_fingerprint"] = event.Fingerprint
		}
		sumUsage(usage, event.Usage)
		for _, delta := range event.Choices {
			if delta.Index < 0 {
				return invalidSSE()
			}
			c := choices[delta.Index]
			if c == nil {
				c = &streamChoice{Index: delta.Index, Message: map[string]any{"role": "assistant", "content": nil}, tools: make(map[int]map[string]any)}
				choices[delta.Index] = c
			}
			if delta.Finish != nil {
				c.Finish = delta.Finish
			}
			for k, v := range delta.Delta {
				switch k {
				case "tool_calls":
					calls, ok := v.([]any)
					if !ok {
						return invalidSSE()
					}
					for _, item := range calls {
						tool, ok := item.(map[string]any)
						if !ok {
							return invalidSSE()
						}
						n, ok := tool["index"].(json.Number)
						if !ok {
							return invalidSSE()
						}
						index, err := n.Int64()
						if err != nil || index < 0 || index > int64(maxAggregateBytes) {
							return invalidSSE()
						}
						dst := c.tools[int(index)]
						if dst == nil {
							dst = make(map[string]any)
							c.tools[int(index)] = dst
						}
						for field, value := range tool {
							if field == "index" {
								continue
							}
							if field == "function" {
								f, ok := value.(map[string]any)
								if !ok {
									return invalidSSE()
								}
								existing, _ := dst[field].(map[string]any)
								if existing == nil {
									existing = make(map[string]any)
									dst[field] = existing
								}
								appendFields(existing, f)
							} else {
								dst[field] = value
							}
						}
					}
				case "function_call":
					f, ok := v.(map[string]any)
					if !ok {
						return invalidSSE()
					}
					existing, _ := c.Message[k].(map[string]any)
					if existing == nil {
						existing = make(map[string]any)
						c.Message[k] = existing
					}
					appendFields(existing, f)
				default:
					appendFields(c.Message, map[string]any{k: v})
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := consume(); err != nil {
				return nil, err
			}
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scanner.Err() != nil {
		return nil, invalidSSE()
	}
	if !done {
		if err := consume(); err != nil {
			return nil, err
		}
	}
	if !done || len(choices) == 0 {
		return nil, invalidSSE()
	}
	indexes := make([]int, 0, len(choices))
	for i := range choices {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	result := make([]*streamChoice, 0, len(indexes))
	for _, i := range indexes {
		c := choices[i]
		if c.Finish == nil {
			return nil, invalidSSE()
		}
		if len(c.tools) > 0 {
			toolIndexes := make([]int, 0, len(c.tools))
			for index := range c.tools {
				toolIndexes = append(toolIndexes, index)
			}
			sort.Ints(toolIndexes)
			tools := make([]map[string]any, 0, len(toolIndexes))
			for _, index := range toolIndexes {
				tools = append(tools, c.tools[index])
			}
			c.Message["tool_calls"] = tools
		}
		finishFields(c.Message)
		result = append(result, c)
	}
	out["choices"] = result
	if len(usage) > 0 {
		out["usage"] = usage
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, invalidSSE()
	}
	// Marshal cannot expose raw upstream data in errors, even for exotic fields.
	return raw, nil
}
