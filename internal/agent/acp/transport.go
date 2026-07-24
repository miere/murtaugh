package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// rpcOutgoingResponse is a JSON-RPC response Murtaugh writes back to the agent
// when it serves an agent-initiated request (e.g. session/request_permission).
// ID is echoed verbatim as raw JSON so a string- or number-typed id round-trips.
type rpcOutgoingResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// jsonRPCMethodNotFound is the standard JSON-RPC code an agent returns when it
// has no handler registered for a method — e.g. an ACP agent that does not
// implement session/cancel. It is a method-level rejection, raised before the
// params are even validated, which makes it a reliable capability signal.
const jsonRPCMethodNotFound = -32601

// jsonRPCInvalidParams and jsonRPCInternalError are the standard JSON-RPC codes
// we return from the filesystem methods we serve: malformed/forbidden params and
// a failed disk operation respectively. Both resolve the agent's request so its
// Read/Write tool fails fast instead of hanging.
const (
	jsonRPCInvalidParams = -32602
	jsonRPCInternalError = -32603
)

// cancelProbeSessionID is the synthetic session id used to probe session/cancel
// support. A non-interruptible agent rejects the method itself (-32601) before
// looking at the session; an interruptible one accepts the call or reports an
// unknown session, neither of which is method-not-found.
const cancelProbeSessionID = "murtaugh-cancel-probe"

// RPCError is a structured ACP/JSON-RPC error. It preserves the numeric code so
// callers can branch on it (e.g. IsMethodNotFound) instead of matching strings.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("ACP %s error %d: %s", e.Method, e.Code, e.Message)
}

// IsMethodNotFound reports whether err is an RPCError carrying the JSON-RPC
// "method not found" code, i.e. the agent does not implement the method.
func IsMethodNotFound(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == jsonRPCMethodNotFound
}

type rpcNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (c *acpSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	startedAt := time.Now()
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	id := c.nextID.Add(1)
	responseCh := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("ACP client is closed")
	}
	c.pending[id] = responseCh
	encoded, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err == nil {
		_, err = c.stdin.Write(append(encoded, '\n'))
	}
	c.mu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("send ACP request %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case response, ok := <-responseCh:
		if !ok {
			return nil, errors.New("ACP request failed: process closed")
		}
		if response.Error != nil {
			return nil, &RPCError{Method: method, Code: response.Error.Code, Message: response.Error.Message}
		}
		c.log.Info("completed ACP request", "method", method, "duration", time.Since(startedAt))
		return response.Result, nil
	}
}

func (c *acpSession) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(line, &envelope); err != nil {
			c.failAll(fmt.Errorf("decode ACP message: %w", err))
			return
		}
		_, hasID := envelope["id"]
		_, hasMethod := envelope["method"]
		switch {
		case hasID && hasMethod:
			// An agent-initiated *request* (it wants a response): permission
			// prompts, fs/terminal calls. Handle off the read loop so a blocking
			// human approval never stalls delivery for every other conversation.
			payload := append([]byte(nil), line...)
			go c.handleAgentRequest(payload)
		case hasID:
			var response rpcResponse
			if err := json.Unmarshal(line, &response); err != nil {
				c.failAll(fmt.Errorf("decode ACP response: %w", err))
				return
			}
			c.deliverResponse(response)
		default:
			var notification rpcNotification
			if err := json.Unmarshal(line, &notification); err != nil {
				c.failAll(fmt.Errorf("decode ACP notification: %w", err))
				return
			}
			c.deliverNotification(notification)
		}
	}
	if err := scanner.Err(); err != nil {
		c.failAll(fmt.Errorf("read ACP stdout: %w", err))
	}
}

func (c *acpSession) deliverResponse(response rpcResponse) {
	c.mu.Lock()
	ch := c.pending[response.ID]
	delete(c.pending, response.ID)
	c.mu.Unlock()
	if ch != nil {
		ch <- response
		close(ch)
	}
}

// respondResult writes a JSON-RPC success response to the agent, echoing id.
func (c *acpSession) respondResult(id json.RawMessage, result any) {
	c.writeResponse(rpcOutgoingResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// respondError writes a JSON-RPC error response to the agent, echoing id.
func (c *acpSession) respondError(id json.RawMessage, code int, message string) {
	c.writeResponse(rpcOutgoingResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (c *acpSession) writeResponse(resp rpcOutgoingResponse) {
	encoded, err := json.Marshal(resp)
	if err != nil {
		c.log.Warn("encode ACP response", "error", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stdin == nil {
		return
	}
	if _, err := c.stdin.Write(append(encoded, '\n')); err != nil {
		c.log.Warn("write ACP response", "error", err)
	}
}

func (c *acpSession) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		ch <- rpcResponse{ID: id, Error: &rpcError{Code: -1, Message: err.Error()}}
		close(ch)
		delete(c.pending, id)
	}
	if c.active != nil {
		select {
		case c.active.events <- agent.Event{Type: agent.EventError, Error: err}:
		default:
		}
	}
}

func (c *acpSession) drainStderr(reader io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(reader, 64*1024))
}
