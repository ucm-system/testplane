package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MCP 协议版本（stdio 传输）。initialize 时若客户端请求的版本在支持列表内就回显，
// 否则回本实现支持的最新版本——这是 MCP 规范要求的协商方式。
const (
	protocolVersion   = "2025-06-18"
	codeParseError    = -32700
	codeInvalidReq    = -32600
	codeMethodMissing = -32601
	codeInvalidParams = -32602
	codeInternalError = -32603
)

var supportedProtocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// Server 是 stdio 上的最小 MCP server。它无会话状态：initialize 只做能力协商，
// 之后的 tools/list 与 tools/call 每次都直接转发到控制面 REST（见 agent-integration.md §5）。
type Server struct {
	client  *Client
	version string
}

// NewServer 构造 MCP server。client 为 nil 时所有工具调用都返回 isError。
func NewServer(client *Client) *Server {
	return &Server{client: client, version: Version}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve 从 in 逐行读 JSON-RPC 2.0 消息、向 out 逐行写应答，直到 EOF 或读写失败。
// MCP 的 stdio 传输就是"一行一条 JSON 消息"，因此不做 Content-Length 帧。
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if in == nil || out == nil {
		return errors.New("mcp: stdin/stdout 不能为空")
	}
	sc := bufio.NewScanner(in)
	// 请求体允许 1 MiB 的 Manifest，留余量给 JSON 转义与协议字段。
	sc.Buffer(make([]byte, 64<<10), 8<<20)

	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := s.handleMessage(ctx, line, enc); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcp: 读取 stdio 失败: %w", err)
	}
	return nil
}

func (s *Server) handleMessage(ctx context.Context, line []byte, enc *json.Encoder) error {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return enc.Encode(response{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &rpcError{Code: codeParseError, Message: "JSON 解析失败: " + err.Error()},
		})
	}

	// 通知（没有 id）不应答；未知通知也静默忽略（JSON-RPC 2.0 §4.1）。
	if isNotification(req.ID) {
		return nil
	}

	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return enc.Encode(response{
			JSONRPC: "2.0",
			ID:      idOrNull(req.ID),
			Error:   &rpcError{Code: codeInvalidReq, Message: `jsonrpc 必须是 "2.0"`},
		})
	}
	if strings.TrimSpace(req.Method) == "" {
		return enc.Encode(response{
			JSONRPC: "2.0",
			ID:      idOrNull(req.ID),
			Error:   &rpcError{Code: codeInvalidReq, Message: "缺少 method"},
		})
	}

	result, rerr := s.dispatch(ctx, req.Method, req.Params)
	resp := response{JSONRPC: "2.0", ID: idOrNull(req.ID)}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	return enc.Encode(resp)
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.initialize(params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList()}, nil
	case "tools/call":
		return s.toolsCall(ctx, params)
	default:
		return nil, &rpcError{Code: codeMethodMissing, Message: "未知方法: " + method}
	}
}

// initialize 返回能力协商结果。capabilities.tools 为空对象：本实现不主动推送
// tools/list_changed，也不支持 resources / prompts。
func (s *Server) initialize(params json.RawMessage) map[string]any {
	pv := protocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(bytes.TrimSpace(params)) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if supportedProtocolVersions[p.ProtocolVersion] {
		pv = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": pv,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "testplane",
			"version": s.version,
		},
	}
}

func (s *Server) toolsCall(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, invalidParams("tools/call 缺少 params")
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("tools/call 参数非法: " + err.Error())
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, invalidParams("tools/call 缺少 name")
	}
	return s.callTool(ctx, p.Name, p.Arguments)
}

func isNotification(id json.RawMessage) bool {
	trimmed := bytes.TrimSpace(id)
	return len(trimmed) == 0 || string(trimmed) == "null"
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if isNotification(id) {
		return json.RawMessage("null")
	}
	return id
}

func invalidParams(msg string) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: msg}
}
