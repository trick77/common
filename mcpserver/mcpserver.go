// Package mcpserver is a small stateless MCP server over streamable HTTP, JSON responses only: no session, no GET
// stream. It answers the 2026-07-28 revision and the initialize-era ones. A caller describes its tools in a Server
// and mounts New's handler; the protocol, the caps and the result shape are this package's.
package mcpserver

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultMaxBodyBytes bounds one request body; tool arguments are a few strings.
	defaultMaxBodyBytes = 1 << 20
	// defaultToolTimeout bounds one tool call: a calling client gives up at 30 s, this leaves room to still deliver
	// a degraded answer or an error inside that.
	defaultToolTimeout = 25 * time.Second
	// defaultMaxTextBytes caps one tool's text. Clients cut at 32 KB; staying under keeps the cut ours, at a line.
	defaultMaxTextBytes = 24 * 1024
	// defaultToolsTTL is how long a client may cache tools/list and discovery: a static tool set, an hour.
	defaultToolsTTL = time.Hour
	// maxFilesBytes bounds one result's text files together, so a reply of text files stays under the 4 MiB a
	// client caps it at (loom does) and the text always arrives. It is also the default budget for images and for
	// binary files, each their own: a client taking several kinds in one reply sizes its cap to their sum, with
	// images and binary files at 4/3 (base64).
	maxFilesBytes = 3 << 20
)

// Server describes one MCP endpoint. Zero values are the defaults noted per field.
type Server struct {
	Name    string // serverInfo.name, and the server field of the tool call log
	Version string // serverInfo.version
	// Instructions is the usage hint sent on initialize and discovery.
	Instructions string
	// InstructionsFunc, when set, replaces Instructions and is built per request: a hint naming the user's data
	// (peeq's channel list) changes without a restart. Cache inside it; discovery and initialize both call it.
	InstructionsFunc func(ctx context.Context) string
	// Tools in tools/list order.
	Tools []Tool
	// Aliases maps retired tool names to current ones. They answer calls but are never listed: a client that cached
	// the old tools/list keeps working until it refreshes.
	Aliases map[string]string
	// ToolsTTL is how long a 2026-07-28 client may cache tools/list and discovery; 0 is an hour. A tool set that
	// changes per user (InstructionsFunc, DescriptionFunc) wants it short.
	ToolsTTL time.Duration
	// ToolTimeout bounds one tool call; 0 is 25 s.
	ToolTimeout time.Duration
	// MaxTextBytes caps one tool's text, cut at a line and marked; 0 is 24 KiB.
	MaxTextBytes int
	// MaxImageBytes bounds one result's images together; 0 is 3 MiB, so a reply stays under a 4 MiB client cap.
	// A server whose client takes larger replies (loom's artifact servers) raises it.
	MaxImageBytes int
	// MaxBlobBytes bounds one result's binary files (File.Data) together in raw bytes, sent as base64 at 4/3; 0 is
	// 3 MiB. Text files and images keep their own budgets. A server whose client takes larger replies raises it.
	MaxBlobBytes int
	// AllowedOrigin is a base URL whose scheme://host[:port] is the one browser Origin accepted. Empty refuses
	// every request that carries an Origin: server-to-server callers send none.
	AllowedOrigin string
	// Auth wraps the protocol handler after the origin check, so a page on a foreign origin learns nothing, not
	// even whether a guessed token was right. Nil leaves auth to whatever the handler is mounted behind.
	Auth func(http.Handler) http.Handler
	// UserHeader names a request header carrying the caller's user id, put in the tool's context (User). Only
	// trustworthy behind auth that only the sending service passes. Empty: no user.
	UserHeader string
}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Title       string // display name, sent top-level and as annotations.title
	Description string
	// DescriptionFunc, when set, replaces Description in tools/list, built per request.
	DescriptionFunc func(ctx context.Context) string
	Schema          map[string]any // input JSON schema; ObjectSchema builds the usual closed object
	ReadOnly        bool           // annotations.readOnlyHint
	OpenWorld       bool           // annotations.openWorldHint: the tool reaches beyond the server's own data
	// MaxArgBytes bounds this tool's arguments; 0 is 1 MiB. A tool that takes an image as an argument (base64)
	// raises it; every other method and tool keeps the 1 MiB request cap.
	MaxArgBytes int
	Run         func(ctx context.Context, args json.RawMessage) Result
}

// Source is one citable link in a tool result's structuredContent. Image is a preview picture; Product marks
// the link as a product page a client may show as a card.
type Source struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Label   string   `json:"label,omitempty"`
	Snippet string   `json:"snippet,omitempty"`
	Image   string   `json:"image,omitempty"`
	Product *Product `json:"product,omitempty"`
}

// Product is what a shop knows about a product page. A nil Price is no price known (out of the range, on
// request), never zero; Currency is ISO 4217. Rating is the average of RatingCount ratings, 0 none.
type Product struct {
	Price       *float64 `json:"price,omitempty"`
	Currency    string   `json:"currency,omitempty"`
	Rating      float64  `json:"rating,omitempty"`
	RatingCount int      `json:"ratingCount,omitempty"`
}

// File is data a tool hands the client as an MCP embedded resource (loom gives it to run_python for the turn).
// A non-empty Data makes it a binary file: the resource carries blob (standard base64 of Data), Text is ignored,
// and one without a MimeType is left out and named in the text. Otherwise it is a text resource carrying Text.
type File struct {
	Name     string // a plain file name, e.g. strava_123_streams.csv
	MimeType string // text/csv, application/json, application/pdf, ...
	Text     string
	Data     []byte
}

// Image is a picture a tool returns as MCP image content. Meta, when set, describes it in
// structuredContent.images at the image's position; its keys are the client's convention. Images over
// Server.MaxImageBytes, or without a type or bytes, are left out and named in the text.
type Image struct {
	MimeType string
	Data     []byte
	Meta     map[string]any
}

// Result is what a tool produced. Sources[i] belongs to the text's item i+1; nil Sources sends no
// structuredContent.sources, a non-nil empty slice an empty list. Files follow the text as embedded resources,
// images follow the files as image blocks.
type Result struct {
	Text    string
	Sources []Source
	Files   []File
	Images  []Image
	IsError bool
}

// ErrorResult is a failed call the model reads and explains.
func ErrorResult(msg string) Result { return Result{Text: msg, IsError: true} }

type userKey struct{}

// WithUser returns ctx carrying a user id, as the handler does from Server.UserHeader.
func WithUser(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userKey{}, id)
}

// User is the user id a call is made for; "" when none was sent.
func User(ctx context.Context) string {
	id, _ := ctx.Value(userKey{}).(string)
	return id
}

// DecodeArgs unmarshals tool arguments strictly, so a misspelt field is an error the model can read rather than
// a silently ignored filter.
func DecodeArgs(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// ObjectSchema is a closed JSON object schema.
func ObjectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

type handler struct {
	s         Server
	origin    string
	tools     map[string]Tool
	ttlMs     int64
	timeout   time.Duration
	maxText   int
	maxImages int
	maxBlobs  int
	// maxBody is the largest body read at all: the default, or a tool's
	// MaxArgBytes plus the envelope. Each method checks its own limit after.
	maxBody int
}

// argEnvelopeBytes is room for the JSON-RPC envelope around a tool's arguments.
const argEnvelopeBytes = 64 << 10

// New returns the endpoint's handler: origin check, then Auth, then method, size and JSON-RPC handling.
func New(s Server) http.Handler {
	h := &handler{s: s, origin: originOf(s.AllowedOrigin), tools: map[string]Tool{},
		ttlMs: s.ToolsTTL.Milliseconds(), timeout: s.ToolTimeout, maxText: s.MaxTextBytes, maxImages: s.MaxImageBytes,
		maxBlobs: s.MaxBlobBytes, maxBody: defaultMaxBodyBytes}
	if h.maxImages <= 0 {
		h.maxImages = maxFilesBytes
	}
	if h.maxBlobs <= 0 {
		h.maxBlobs = maxFilesBytes
	}
	for _, t := range s.Tools {
		h.maxBody = max(h.maxBody, t.MaxArgBytes+argEnvelopeBytes)
	}
	if h.ttlMs <= 0 {
		h.ttlMs = defaultToolsTTL.Milliseconds()
	}
	if h.timeout <= 0 {
		h.timeout = defaultToolTimeout
	}
	if h.maxText <= 0 {
		h.maxText = defaultMaxTextBytes
	}
	for _, t := range s.Tools {
		h.tools[t.Name] = t
	}
	var inner http.Handler = http.HandlerFunc(h.serve)
	if s.Auth != nil {
		inner = s.Auth(inner)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Before auth: DNS rebinding included, a foreign page gets nothing.
		if o := r.Header.Get("Origin"); o != "" && (h.origin == "" || !strings.EqualFold(o, h.origin)) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// originOf reduces a base URL to scheme://host[:port], or "" when it is not an absolute URL.
func originOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Stateless and JSON-only: no GET stream to open, no session to DELETE.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(h.maxBody)))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeRPCError(w, http.StatusBadRequest, nil, codeParseError, "could not read the request body", nil)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, "batch requests are not supported", nil)
		return
	}
	var req request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, codeParseError, "parse error", nil)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, codeInvalidRequest, "invalid request", nil)
		return
	}
	// Only a tool call may be larger, and only for a tool that asks for it (callTool checks which).
	if req.Method != "tools/call" && len(body) > defaultMaxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	h.dispatch(w, r, req)
}

func (h *handler) instructions(ctx context.Context) string {
	if h.s.InstructionsFunc != nil {
		return h.s.InstructionsFunc(ctx)
	}
	return h.s.Instructions
}

func (h *handler) toolDefs(ctx context.Context) []map[string]any {
	out := make([]map[string]any, 0, len(h.s.Tools))
	for _, t := range h.s.Tools {
		desc := t.Description
		if t.DescriptionFunc != nil {
			desc = t.DescriptionFunc(ctx)
		}
		// The title goes out twice: top-level for the current spec, annotations.title for clients that still read
		// the older place. Dropping either loses the title for those clients.
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": desc,
			"inputSchema": t.Schema,
			"annotations": map[string]any{"title": t.Title, "readOnlyHint": t.ReadOnly, "openWorldHint": t.OpenWorld},
		})
	}
	return out
}

func (h *handler) callTool(w http.ResponseWriter, r *http.Request, req request, modern bool) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "tools/call needs a tool name", nil)
		return
	}
	t, ok := h.tools[p.Name]
	if !ok {
		t, ok = h.tools[h.s.Aliases[p.Name]]
	}
	if !ok {
		// -32602 on HTTP 200: a dual-era client reads any other 4xx as "legacy server" and falls back.
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "unknown tool: "+p.Name, nil)
		return
	}
	if limit := cmp.Or(t.MaxArgBytes, defaultMaxBodyBytes); len(p.Arguments) > limit {
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "arguments too large for "+p.Name, nil)
		return
	}
	if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
		p.Arguments = json.RawMessage("{}")
	}
	ctx := r.Context()
	if h.s.UserHeader != "" {
		ctx = WithUser(ctx, strings.TrimSpace(r.Header.Get(h.s.UserHeader)))
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	start := time.Now()
	res := t.Run(ctx, p.Arguments)
	files, dropped, blobBytes := h.fileContent(res.Files)
	images, metas, droppedImages, imageBytes := h.imageContent(p.Name, res.Images)
	dropped = append(dropped, droppedImages...)
	slog.Info("mcp tool call", "server", h.s.Name, "tool", p.Name, "is_error", res.IsError,
		"ms", time.Since(start).Milliseconds(), "files", len(files), "blob_bytes", blobBytes,
		"images", len(images), "image_bytes", imageBytes, "not_attached", len(dropped))

	text := res.Text
	if len(dropped) > 0 {
		// Capped first, so the note survives a long text: without it the model would ask for a file it never got.
		note := "\n(not attached: " + strings.Join(dropped, ", ") + ")"
		text = CapText(text, h.maxText-len(note)) + note
	} else {
		text = CapText(text, h.maxText)
	}
	// Order is part of the shape: text, then files in Result.Files order (each a text or a blob resource), then
	// images (structuredContent.images[i] is the i-th image).
	content := append(append([]map[string]any{{"type": "text", "text": text}}, files...), images...)
	structured := map[string]any{}
	if res.Sources != nil {
		structured["sources"] = res.Sources
	}
	if len(metas) > 0 {
		structured["images"] = metas
	}
	result := map[string]any{"content": content}
	if len(structured) > 0 {
		result["structuredContent"] = structured
	}
	if res.IsError {
		result["isError"] = true
	}
	if modern {
		result = h.modern(result)
	}
	writeResult(w, req.ID, result)
}

// imageContent turns images into image blocks, in order, while they fit maxImages together, with their meta. Each
// one left out (no type or bytes, over the budget) is named "image N" for the text. A meta that JSON cannot encode
// (NaN, a func) goes out empty: the reply is written after its 200, so a failed encode would lose every image.
func (h *handler) imageContent(tool string, imgs []Image) (blocks, metas []map[string]any, dropped []string, used int) {
	for i, img := range imgs {
		label := "image " + strconv.Itoa(i+1)
		switch {
		case img.MimeType == "" || len(img.Data) == 0:
			dropped = append(dropped, label+" (empty)")
			continue
		case used+len(img.Data) > h.maxImages:
			dropped = append(dropped, label+" (too large)")
			continue
		}
		used += len(img.Data)
		// []byte: encoding/json writes standard base64 itself, no extra copy as a string.
		blocks = append(blocks, map[string]any{"type": "image", "mimeType": img.MimeType, "data": img.Data})
		meta := img.Meta
		if _, err := json.Marshal(meta); err != nil {
			slog.Warn("mcp image meta not encodable, sent empty", "server", h.s.Name, "tool", tool, "image", i+1, "err", err)
			meta = nil
		}
		if meta == nil {
			meta = map[string]any{}
		}
		metas = append(metas, meta)
	}
	return blocks, metas, dropped, used
}

// fileContent turns files into embedded resources, in order: text files while they fit maxFilesBytes together,
// binary files while they fit maxBlobs together, each budget its own. Each one left out (over its budget, a binary
// file without a type) is named for the text. The uri is the plain name unescaped, as text files always had it.
func (h *handler) fileContent(files []File) (out []map[string]any, dropped []string, blobBytes int) {
	textBytes := 0
	for _, f := range files {
		// []byte: encoding/json writes standard base64 itself, the MCP blob encoding.
		used, limit, key, val, size := &textBytes, maxFilesBytes, "text", any(f.Text), len(f.Text)
		if len(f.Data) > 0 {
			used, limit, key, val, size = &blobBytes, h.maxBlobs, "blob", f.Data, len(f.Data)
			if f.MimeType == "" {
				dropped = append(dropped, f.Name+" (no type)")
				continue
			}
		}
		if *used+size > limit {
			dropped = append(dropped, f.Name+" (too large)")
			continue
		}
		*used += size
		out = append(out, map[string]any{"type": "resource", "resource": map[string]any{
			"uri": "file:///" + f.Name, "mimeType": f.MimeType, key: val,
		}})
	}
	return out, dropped, blobBytes
}
