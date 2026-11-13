package llmwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Probes for the Anthropic models on Anthropic's own API (wire_api: messages). The
// typed ones run through FromEnv, so they need exactly what an application
// supplies, LLMWIRE_ANTHROPIC_API_KEY; the raw ones post hand-built bodies
// below plan, to read what plan refuses. messages.go head comment and the
// profile say which bit each one backs.

// messagesModels are the profiles every Messages probe runs against, one
// subtest each: -run 'Probe_Messages.*/claude-fable-5-2' probes one model.
var messagesModels = []string{"claude-opus-5-5", "claude-fable-5-2"}

// eachMessagesModel runs fn once per model in messagesModels, as a subtest.
func eachMessagesModel(t *testing.T, fn func(t *testing.T, model string)) {
	t.Helper()
	for _, model := range messagesModels {
		t.Run(model, func(t *testing.T) { fn(t, model) })
	}
}

// messagesClient is a FromEnv client for one Messages profile, or a skip.
func messagesClient(t *testing.T, model string) *Client {
	t.Helper()
	evalGate(t)
	c, err := FromEnv(model, Config{
		HeaderTimeout: 90 * time.Second,
		IdleTimeout:   90 * time.Second,
		CallTimeout:   5 * time.Minute,
	})
	if err != nil {
		t.Skipf("live probe: %v", err)
	}
	return c
}

// messagesChat is one metered typed call.
func messagesChat(t *testing.T, c *Client, model string, req ChatRequest) (*ChatResponse, []Warning, error) {
	t.Helper()
	req.Model = model
	theMeter.reserve(t, model)
	resp, warnings, err := c.Chat(context.Background(), req)
	if resp != nil {
		theMeter.record(model, resp.Usage)
	}
	return resp, warnings, err
}

// messagesRaw posts a hand-built body to /messages, metered, and returns the
// status the endpoint answered with: 200 or the APIError's.
func messagesRaw(t *testing.T, model string, b body) (json.RawMessage, *APIError) {
	t.Helper()
	c := anthropicEndpoint.client(t)
	b["model"] = model
	if _, ok := b["max_tokens"]; !ok {
		b["max_tokens"] = 256
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	theMeter.reserve(t, model)
	resp, _, err := c.RawPost(context.Background(), routeMessages, raw)
	if err != nil {
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("FINDING transport failure: %v", err)
		}
		return nil, apiErr
	}
	theMeter.record(model, parseMessagesUsage(func() json.RawMessage {
		var w wireMessage
		_ = json.Unmarshal(resp, &w)
		return w.Usage
	}()))
	return resp, nil
}

func userTurn(text string) []any {
	return []any{map[string]any{"role": "user", "content": text}}
}

// TestProbe_MessagesBasics: a system turn, the answer, usage on the
// protocol's lanes, the cost from the table, the uncapped call going out at
// max_output.
func TestProbe_MessagesBasics(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		resp, warnings, err := messagesChat(t, c, model, ChatRequest{
			Messages: []Message{System("Answer in one short sentence."), User("What is the capital of Switzerland?")},
		})
		if err != nil {
			t.Fatalf("FINDING plain uncapped Chat REJECTED: %v", err)
		}
		t.Logf("content %q reasoning %d chars finish %q model %q warnings %v", resp.Content, len(resp.Reasoning),
			resp.FinishReason, resp.Model, warnings)
		t.Logf("usage raw %s; in total %d nocache %d read %d write %d out %d; cost %+v; timing %+v",
			resp.Usage.Raw, Tokens(resp.Usage.Input.Total), Tokens(resp.Usage.Input.NoCache), Tokens(resp.Usage.Input.CacheRead),
			Tokens(resp.Usage.Input.CacheWrite), Tokens(resp.Usage.Output.Total), resp.Usage.Cost, resp.Timing)
		if !strings.Contains(resp.Content, "Bern") || resp.FinishReason != "stop" {
			t.Errorf("FINDING content %q finish %q", resp.Content, resp.FinishReason)
		}
		if resp.Usage.Cost.Provenance != FromTable || resp.Usage.Cost.NanoUSD <= 0 {
			t.Errorf("FINDING not priced from the table: %+v", resp.Usage.Cost)
		}
	})
}

// TestProbe_MessagesEffortLadder: every level in effort_values accepted, and a
// level outside it (none, minimal) refused by the endpoint as plan refuses it.
func TestProbe_MessagesEffortLadder(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
			resp, _, err := messagesChat(t, c, model, ChatRequest{
				Messages:  []Message{User("Reply with the single word: ok")},
				Reasoning: ReasoningEffort(level), MaxTokens: ptr(2000),
			})
			if err != nil {
				t.Errorf("FINDING effort %q REJECTED: %v", level, err)
				continue
			}
			t.Logf("effort %s: content %q out %d reasoning chars %d", level, resp.Content,
				Tokens(resp.Usage.Output.Total), len(resp.Reasoning))
		}
		for _, level := range []string{"none", "minimal"} {
			_, apiErr := messagesRaw(t, model, body{"messages": userTurn("Reply with ok"),
				"output_config": map[string]any{"effort": level}})
			if apiErr == nil {
				t.Errorf("FINDING effort %q ACCEPTED by the endpoint; effort_values is narrower than the API", level)
				continue
			}
			t.Logf("effort %s refused: status %d code %q message %q", level, apiErr.StatusCode, apiErr.Code, apiErr.Message)
		}
	})
}

// TestProbe_MessagesRefusals: the knobs the profile marks unsupported, each sent
// raw. A 200 on any of them means the profile refuses something the endpoint
// takes.
func TestProbe_MessagesRefusals(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		cases := []struct {
			name string
			b    body
		}{
			{"thinking disabled", body{"thinking": map[string]any{"type": "disabled"}}},
			{"budget_tokens", body{"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}},
			{"temperature", body{"temperature": 0.5}},
			{"top_p", body{"top_p": 0.9}},
			{"tool_choice any", body{"tool_choice": map[string]any{"type": "any"}, "tools": []any{weatherToolRaw()}}},
			{"tool_choice tool", body{"tool_choice": map[string]any{"type": "tool", "name": "get_weather"},
				"tools": []any{weatherToolRaw()}}},
		}
		for _, tc := range cases {
			tc.b["messages"] = userTurn("Reply with ok")
			_, apiErr := messagesRaw(t, model, tc.b)
			if apiErr == nil {
				t.Errorf("FINDING %s ACCEPTED (200)", tc.name)
				continue
			}
			t.Logf("%s refused: status %d code %q type %q message %q", tc.name, apiErr.StatusCode, apiErr.Code,
				apiErr.Type, apiErr.Message)
			if apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "invalid_request_error" {
				t.Errorf("FINDING %s: status %d code %q, expected 400 invalid_request_error", tc.name, apiErr.StatusCode, apiErr.Code)
			}
		}
		// An unknown model: the error body's shape, and that error.type lands in
		// Code.
		c := anthropicEndpoint.client(t)
		theMeter.reserve(t, model)
		_, _, err := c.RawPost(context.Background(), routeMessages,
			[]byte(`{"model":"claude-no-such-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("FINDING unknown model: %v", err)
		}
		t.Logf("unknown model: status %d code %q message %q", apiErr.StatusCode, apiErr.Code, apiErr.Message)
		if apiErr.Code == "" {
			t.Errorf("FINDING unknown model: no code decoded from the error body")
		}
	})
}

func weatherToolRaw() map[string]any {
	return map[string]any{"name": "get_weather", "description": "Current weather for a city",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required": []any{"city"}}}
}

// TestProbe_MessagesToolLoop is the protocol end to end: a tool round that makes
// the model think, the round that replays its signed thinking, the same first
// round streamed, and a hand-built assistant turn (no ResponseItems) replayed.
func TestProbe_MessagesToolLoop(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		req := ChatRequest{
			Model: model,
			Messages: []Message{System("You are a terse assistant. Use tools when asked."), User("Think carefully first: take the number of cantons in Switzerland, " +
				"multiply by the number of letters in its capital, subtract 70. If the result is even, the city " +
				"is Bern, otherwise Basel. Then call get_weather for that city. Do not answer in prose.")},
			Tools:      []Tool{weatherTool},
			ToolChoice: ToolChoice{Mode: ToolChoiceRequired},
			Reasoning:  ReasoningEffort("high"),
			MaxTokens:  ptr(8000),
		}
		r1, warnings, err := messagesChat(t, c, model, req)
		if err != nil {
			t.Fatalf("FINDING round 1 REJECTED: %v", err)
		}
		t.Logf("round 1: calls %+v, items %d, reasoning %d chars, finish %q, usage raw %s, warnings %v",
			r1.ToolCalls, len(r1.ResponseItems), len(r1.Reasoning), r1.FinishReason, r1.Usage.Raw, warnings)
		for i, it := range r1.ResponseItems {
			t.Logf("  item %d: %s", i, Truncate(string(it), 160))
		}
		if len(r1.ToolCalls) != 1 || r1.FinishReason != "tool_calls" {
			t.Fatalf("FINDING round 1: no single tool call, finish %q", r1.FinishReason)
		}
		if r1.Reasoning == "" {
			t.Errorf("FINDING round 1: no summarized thinking text")
		}

		req.Messages = append(req.Messages, r1.Assistant(),
			ToolResult(r1.ToolCalls[0].ID, `{"city":"Bern","temp_c":14,"sky":"overcast"}`))
		r2, _, err := messagesChat(t, c, model, req)
		if err != nil {
			t.Fatalf("FINDING round 2 (thinking replay) REJECTED: %v", err)
		}
		t.Logf("round 2: content %q, finish %q, in %d out %d", r2.Content, r2.FinishReason,
			Tokens(r2.Usage.Input.Total), Tokens(r2.Usage.Output.Total))
		if r2.Content == "" {
			t.Errorf("FINDING round 2: no answer after the tool result")
		}

		// The same round 2 with the assistant turn rebuilt by hand: no thinking
		// block in front of the tool_use. Documented as required while thinking
		// is on for the turn in progress.
		hand := append([]Message(nil), req.Messages[:2]...)
		hand = append(hand, Message{Role: RoleAssistant, ToolCalls: r1.ToolCalls},
			ToolResult(r1.ToolCalls[0].ID, `{"city":"Bern","temp_c":14,"sky":"overcast"}`))
		r3, _, err := messagesChat(t, c, model, ChatRequest{Messages: hand, Tools: req.Tools, Reasoning: ReasoningEffort("low"), MaxTokens: ptr(4000)})
		if err != nil {
			t.Logf("FINDING hand-built tool turn (no thinking block) REJECTED: %v", err)
		} else {
			t.Logf("hand-built tool turn accepted: content %q", r3.Content)
		}

		theMeter.reserve(t, model)
		req.Messages = req.Messages[:2]
		s, _, err := c.ChatStream(context.Background(), req)
		if err != nil {
			t.Fatalf("FINDING stream REJECTED: %v", err)
		}
		defer func() { _ = s.Close() }()
		var kinds []string
		for s.Next() {
			ev := s.Event()
			if n := len(kinds); n == 0 || kinds[n-1] != string(ev.Kind) {
				kinds = append(kinds, string(ev.Kind))
			}
		}
		res, err := s.Result(), s.Err()
		theMeter.record(model, res.Usage)
		if err != nil {
			t.Fatalf("FINDING stream failed: %v", err)
		}
		t.Logf("stream: kinds %v, calls %+v, items %d, reasoning %d chars, events %d, first data %s, max data gap %s of %s, usage raw %s, cost %+v",
			kinds, res.ToolCalls, len(res.ResponseItems), len(res.Reasoning), res.Events, res.Timing.FirstData,
			res.MaxDataGap, res.Timing.Total, res.Usage.Raw, res.Usage.Cost)
		var args map[string]any
		if len(res.ToolCalls) != 1 || json.Unmarshal([]byte(res.ToolCalls[0].Arguments), &args) != nil {
			// Fatal: the replay below indexes the call, and a panic would
			// stop every later model's subtest.
			t.Fatalf("FINDING stream: tool call %+v does not carry one JSON argument object", res.ToolCalls)
		}
		if res.Usage.Cost.Provenance != FromTable {
			t.Errorf("FINDING stream unpriced: %+v", res.Usage.Cost)
		}
		// The streamed turn replays too.
		req.Messages = append(req.Messages, res.Assistant(),
			ToolResult(res.ToolCalls[0].ID, `{"city":"Bern","temp_c":14,"sky":"overcast"}`))
		r4, _, err := messagesChat(t, c, model, req)
		if err != nil {
			t.Fatalf("FINDING streamed turn replay REJECTED: %v", err)
		}
		t.Logf("streamed replay: content %q", r4.Content)
	})
}

// TestProbe_MessagesCacheContainment: two identical prompts past the cache
// minimum, with top-level cache_control through ExtraBody. Settles whether
// cache reads and writes sit BESIDE input_tokens (the protocol's documented
// shape, which usage() adds up) and that the write is billed on the 5m lane.
func TestProbe_MessagesCacheContainment(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		long := strings.Repeat("The quick brown fox jumps over the lazy dog near the riverbank at dawn. ", 400)
		req := ChatRequest{
			Messages:  []Message{System(long), User("Reply with the single word: ok")},
			Reasoning: ReasoningEffort("low"), MaxTokens: ptr(500),
			ExtraBody: map[string]any{"cache_control": map[string]any{"type": "ephemeral"}},
		}
		var got [2]*ChatResponse
		for i := range got {
			resp, _, err := messagesChat(t, c, model, req)
			if err != nil {
				t.Fatalf("FINDING call %d REJECTED: %v", i+1, err)
			}
			got[i] = resp
			u := resp.Usage
			t.Logf("call %d: raw %s; total %d nocache %d read %d write %d; cost %+v", i+1, u.Raw,
				Tokens(u.Input.Total), Tokens(u.Input.NoCache), Tokens(u.Input.CacheRead), Tokens(u.Input.CacheWrite), u.Cost)
		}
		a, b := got[0].Usage, got[1].Usage
		if Tokens(a.Input.CacheWrite) == 0 || Tokens(b.Input.CacheRead) == 0 {
			t.Fatalf("FINDING no cache write then read: write %d read %d", Tokens(a.Input.CacheWrite), Tokens(b.Input.CacheRead))
		}
		// Beside: the prompt is the same bytes, so the totals agree while
		// input_tokens drops by what moved into the cache lanes.
		if Tokens(a.Input.Total) != Tokens(b.Input.Total) || Tokens(b.Input.NoCache) >= Tokens(a.Input.NoCache)+Tokens(a.Input.CacheWrite) {
			t.Errorf("FINDING containment: totals %d vs %d, nocache %d vs %d", Tokens(a.Input.Total), Tokens(b.Input.Total),
				Tokens(a.Input.NoCache), Tokens(b.Input.NoCache))
		}
	})
}

// TestProbe_MessagesConversationShapes: the conversation shapes checkMessagesTurns
// refuses or lets through. Each is reported, not asserted: what the endpoint
// answers is the finding.
func TestProbe_MessagesConversationShapes(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		user := func(s string) map[string]any { return map[string]any{"role": "user", "content": s} }
		asst := func(s string) map[string]any { return map[string]any{"role": "assistant", "content": s} }
		for name, msgs := range map[string][]any{
			"assistant first":     {asst("Hello, I am ready."), user("Reply with ok")},
			"prefill":             {user("Give a JSON object with key a=1"), asst("{")},
			"whitespace user":     {user(" "), asst("ok"), user("Reply with ok")},
			"trailing whitespace": {user("Reply with ok"), asst("ok ")},
			"two assistants":      {user("Reply with ok"), asst("ok"), asst("ok again"), user("Reply with ok")},
			"whitespace tool result": {user("Call get_weather for Bern"),
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_x",
					"name": "get_weather", "input": map[string]any{"city": "Bern"}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_x",
					"content": " \n"}}}},
			"text before tool result": {user("Call get_weather for Bern"),
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_x",
					"name": "get_weather", "input": map[string]any{"city": "Bern"}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Here it is."},
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_x", "content": "14C"}}}},
			"assistant before tool result": {user("Call get_weather for Bern"),
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_x",
					"name": "get_weather", "input": map[string]any{"city": "Bern"}}}},
				asst("Calling now."),
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_x",
					"content": "14C"}}}},
			"empty tool result": {user("Call get_weather for Bern"),
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_x",
					"name": "get_weather", "input": map[string]any{"city": "Bern"}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_x",
					"content": ""}}}},
		} {
			b := body{"messages": msgs}
			if strings.Contains(name, "tool result") {
				// As a tool loop sends it: the tool defined beside its call.
				b["tools"] = []any{weatherToolRaw()}
			}
			raw, apiErr := messagesRaw(t, model, b)
			if apiErr != nil {
				t.Logf("%s: REFUSED status %d code %q message %q", name, apiErr.StatusCode, apiErr.Code, apiErr.Message)
				continue
			}
			var w wireMessage
			_ = json.Unmarshal(raw, &w)
			t.Logf("%s: ACCEPTED stop %q content %s", name, w.StopReason, Truncate(string(raw), 300))
		}
	})
}

// TestProbe_MessagesSignedJoins: where the endpoint joins consecutive assistant
// turns, with real signed thinking. A plain note after a replayed calling
// turn goes through plan and must be accepted. A replayed turn sent twice
// also goes through plan, and must come back as the endpoint's signature 400.
func TestProbe_MessagesSignedJoins(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		req := ChatRequest{Model: model, Tools: []Tool{weatherTool}, Reasoning: ReasoningEffort("high"), MaxTokens: ptr(4000),
			Messages: []Message{User("Think step by step about which Swiss city has more cantons around it, then call get_weather for Bern.")}}
		r1, _, err := messagesChat(t, c, model, req)
		if err != nil || len(r1.ToolCalls) != 1 {
			t.Fatalf("round 1: %v %+v", err, r1)
		}
		result := ToolResult(r1.ToolCalls[0].ID, `{"temp_c":14}`)
		r2, _, err := messagesChat(t, c, model, ChatRequest{Model: model, Tools: req.Tools, Reasoning: req.Reasoning, MaxTokens: req.MaxTokens,
			Messages: append(append([]Message(nil), req.Messages...), r1.Assistant(), result)})
		if err != nil {
			t.Fatalf("round 2: %v", err)
		}
		// Same thinking settings as the turns being replayed, so only the shape
		// varies.
		note, _, err := messagesChat(t, c, model, ChatRequest{Model: model, Tools: req.Tools, Reasoning: req.Reasoning,
			MaxTokens: req.MaxTokens, Messages: []Message{req.Messages[0], r1.Assistant(), Assistant("Checking the weather now."), result}})
		if err != nil {
			t.Errorf("FINDING note after signed call REFUSED: %v", err)
		} else {
			t.Logf("note after signed call: ACCEPTED %q", Truncate(note.Content, 120))
		}

		// The same request with the second round's turn sent twice: plan lets it
		// through (it cannot tell an edit), and the endpoint's 400 must come back
		// as an APIError naming the signature, not as anything else.
		_, _, err = messagesChat(t, c, model, ChatRequest{Model: model, Tools: req.Tools, Reasoning: req.Reasoning, MaxTokens: req.MaxTokens,
			Messages: []Message{req.Messages[0], r1.Assistant(), result, r2.Assistant(), r2.Assistant(), User("Say it again.")}})
		var apiErr *APIError
		switch {
		case err == nil:
			t.Logf("FINDING a signed turn sent twice was ACCEPTED")
		case !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || !strings.Contains(apiErr.Message, "signature"):
			t.Errorf("FINDING a signed turn sent twice failed some other way: %v", err)
		default:
			t.Logf("signed turn sent twice: REFUSED status %d code %q message %q", apiErr.StatusCode, apiErr.Code, apiErr.Message)
		}
	})
}

// TestProbe_MessagesStructuredOutput: output_config.format json_schema.
func TestProbe_MessagesStructuredOutput(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		resp, _, err := messagesChat(t, c, model, ChatRequest{
			Messages: []Message{User("Name the capital of Switzerland and its canton.")},
			ResponseFormat: ResponseFormat{Kind: FormatJSONSchema, Name: "capital", Strict: true, Schema: map[string]any{
				"type": "object", "additionalProperties": false, "required": []any{"city", "canton"},
				"properties": map[string]any{"city": map[string]any{"type": "string"}, "canton": map[string]any{"type": "string"}}}},
			Reasoning: ReasoningEffort("low"), MaxTokens: ptr(2000),
		})
		if err != nil {
			t.Fatalf("FINDING json_schema REJECTED: %v", err)
		}
		var v map[string]string
		if err := json.Unmarshal([]byte(resp.Content), &v); err != nil || v["city"] == "" {
			t.Errorf("FINDING json_schema content %q does not parse: %v", resp.Content, err)
		}
		t.Logf("json_schema: %s", resp.Content)
	})
}

// TestProbe_MessagesStreamFrames reads a long think's raw SSE: which frames come,
// how often ping arrives, and the longest gap between non-ping data frames,
// which is what the idle guard sees with display summarized. MEASURED
// 2026-10-26 at max_tokens 16000: 1144 thinking_delta frames over 2m40s, the
// longest gap 4.8s, ONE ping in the whole stream; it stopped on max_tokens
// with all 16000 output tokens spent thinking (about $0.32). The cap is 4000
// now: the gaps are the finding, not the answer.
func TestProbe_MessagesStreamFrames(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := anthropicEndpoint.client(t)
		raw := []byte(`{"model":"` + model + `","max_tokens":4000,"stream":true,` +
			`"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"},` +
			`"messages":[{"role":"user","content":"How many integers n between 1 and 2000 make n^2+n+41 composite? ` +
			`Work it out carefully, then give only the number."}]}`)
		theMeter.reserve(t, model)
		x, _, _, err := c.openStream(context.Background(), routeMessages, raw)
		if err != nil {
			t.Fatalf("FINDING stream REJECTED: %v", err)
		}
		defer x.release()
		defer func() { _ = x.resp.Body.Close() }()
		var (
			counts           = map[string]int{}
			lastData, start  = time.Now(), time.Now()
			maxGap, maxPing  time.Duration
			lastPing         = time.Now()
			eventLines, rest int
		)
		sc := bufio.NewScanner(x.resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), maxStreamLine)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				eventLines++
			case strings.HasPrefix(line, dataPrefix):
				var ev messagesEvent
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, dataPrefix)), &ev)
				key := ev.Type
				if ev.Delta.Type != "" {
					key += "/" + ev.Delta.Type
				}
				counts[key]++
				now := time.Now()
				if ev.Type == "ping" {
					maxPing = max(maxPing, now.Sub(lastPing))
					lastPing = now
					continue
				}
				maxGap = max(maxGap, now.Sub(lastData))
				lastData = now
				x.guard.arm(c.idle, stallIdle)
				if ev.Type == "message_delta" {
					t.Logf("message_delta: %s", line)
					theMeter.record(model, parseMessagesUsage(ev.Usage))
				}
			case line != "":
				rest++
			}
		}
		t.Logf("frames %v, event lines %d, other lines %d, longest non-ping gap %s, longest ping interval %s, total %s, scan err %v",
			counts, eventLines, rest, maxGap, maxPing, time.Since(start), sc.Err())
	})
}

// TestProbe_MessagesModelsListing: GET /models with the key header and version.
func TestProbe_MessagesModelsListing(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		entries, warnings, err := c.ListModels(context.Background())
		if err != nil {
			t.Fatalf("FINDING listing failed: %v", err)
		}
		var entry *ModelEntry
		for i := range entries {
			if entries[i].ID == model {
				entry = &entries[i]
			}
		}
		t.Logf("listing: %d entries, warnings %v", len(entries), warnings)
		if entry == nil {
			t.Fatalf("FINDING %s not in the listing", model)
		}
		t.Logf("%s: max_input %d max_output %d raw %s", model, Tokens(entry.MaxInputTokens), Tokens(entry.MaxOutputTokens),
			Truncate(string(entry.Raw), 400))
	})
}

// TestProbe_MessagesEditedHistory: what a caller that edits the conversation
// between rounds gets. A replayed signed turn after the system prompt changed
// (a client appending a directive, hoisted into the top-level system) is the
// history-editing check; the same edit with the assistant turn rebuilt, no
// thinking block, removes every block and must pass.
func TestProbe_MessagesEditedHistory(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		req := ChatRequest{Tools: []Tool{weatherTool}, Reasoning: ReasoningEffort("high"), MaxTokens: ptr(4000),
			Messages: []Message{System("You are a terse assistant."),
				User("Think briefly about which Swiss city is the capital, then call get_weather for it.")}}
		r1, _, err := messagesChat(t, c, model, req)
		if err != nil || len(r1.ToolCalls) != 1 {
			t.Fatalf("round 1: %v %+v", err, r1)
		}
		result := ToolResult(r1.ToolCalls[0].ID, `{"temp_c":14}`)
		edited := []Message{System("You are a terse assistant. Answer in German."), req.Messages[1]}

		_, _, err = messagesChat(t, c, model, ChatRequest{Tools: req.Tools, Reasoning: req.Reasoning, MaxTokens: req.MaxTokens,
			Messages: append(append([]Message(nil), edited...), r1.Assistant(), result)})
		var apiErr *APIError
		switch {
		case err == nil:
			t.Logf("FINDING signed turn replayed after a system edit: ACCEPTED")
		case errors.As(err, &apiErr):
			t.Logf("signed turn replayed after a system edit: REFUSED status %d code %q message %q",
				apiErr.StatusCode, apiErr.Code, apiErr.Message)
		default:
			t.Errorf("FINDING system edit failed some other way: %v", err)
		}

		r3, _, err := messagesChat(t, c, model, ChatRequest{Tools: req.Tools, Reasoning: req.Reasoning, MaxTokens: req.MaxTokens,
			Messages: append(append([]Message(nil), edited...), Message{Role: RoleAssistant, ToolCalls: r1.ToolCalls}, result)})
		if err != nil {
			t.Errorf("FINDING rebuilt turn (no thinking) after a system edit REFUSED: %v", err)
		} else {
			t.Logf("rebuilt turn after a system edit: ACCEPTED %q", Truncate(r3.Content, 120))
		}
	})
}

// TestProbe_MessagesDeepOverhead: thinking tokens at the profile's deep level
// on chat-shaped prompts, uncapped in practice (32000), to size
// reasoning.overhead for it. A sample, not a distribution: the figures go in
// the profile comment.
func TestProbe_MessagesDeepOverhead(t *testing.T) {
	eachMessagesModel(t, func(t *testing.T, model string) {
		c := messagesClient(t, model)
		for name, prompt := range map[string]string{
			"factual": "What is the capital of Switzerland, and since when?",
			"explain": "Explain in about 150 words why SQLite's WAL mode lets readers and a writer run at the same time.",
			"puzzle": "A train leaves Zurich at 08:00 at 120 km/h, another leaves Bern (100 km away) at 08:15 towards " +
				"Zurich at 80 km/h. When and where do they meet? Give the time and the distance from Zurich.",
		} {
			resp, _, err := messagesChat(t, c, model, ChatRequest{
				Messages:  []Message{System("You are a helpful assistant. Answer concisely."), User(prompt)},
				Reasoning: ReasoningDeep(), MaxTokens: ptr(32000),
			})
			if err != nil {
				t.Errorf("FINDING %s: %v", name, err)
				continue
			}
			t.Logf("deep %s: sent %q out %d thinking %d finish %q total %s", name, resp.ReasoningSent,
				Tokens(resp.Usage.Output.Total), Tokens(resp.Usage.Output.Reasoning), resp.FinishReason, resp.Timing.Total)
		}
	})
}
