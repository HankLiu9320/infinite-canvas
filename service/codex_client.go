package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tigerowo/infinite-canvas/config"
	"github.com/tigerowo/infinite-canvas/repository"
)

const ModelChannelProtocolCodex = "codex"

type codexRPCMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type codexListener struct {
	events chan codexRPCMessage
	done chan struct{}
	once sync.Once
	err error
}

func (listener *codexListener) fail(err error) {
	listener.once.Do(func() { listener.err = err; close(listener.done) })
}

type codexClient struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan codexRPCMessage
	listeners map[string]*codexListener
	nextID    atomic.Uint64
	done      chan struct{}
	once      sync.Once
	err       error
}

var codexRuntime = struct {
	sync.Mutex
	ctx    context.Context
	client *codexClient
}{ctx: context.Background()}

func StartCodexService(ctx context.Context) error {
	codexRuntime.Lock()
	codexRuntime.ctx = ctx
	codexRuntime.Unlock()
	settings, err := repository.GetSettings()
	if err != nil {
		return err
	}
	ids := []string{}
	for _, channel := range settings.Private.Channels {
		if IsCodexChannel(channel) {
			ids = append(ids, channel.ID)
		}
	}
	return repository.FailInterruptedCodexImageTasks(ids, time.Now().UTC().Format(time.RFC3339Nano), "后端已重启，Codex 任务中断，请重新生成")
}

func CloseCodexService() {
	codexRuntime.Lock()
	client := codexRuntime.client
	codexRuntime.Unlock()
	if client != nil {
		client.close(errors.New("Codex 服务已停止"))
	}
}

func getCodexClient(ctx context.Context) (*codexClient, error) {
	if !config.Cfg.CodexEnabled {
		return nil, safeMessageError{message: "后端未启用 Codex，请设置 CODEX_ENABLED=true"}
	}
	codexRuntime.Lock()
	defer codexRuntime.Unlock()
	if client := codexRuntime.client; client != nil {
		select {
		case <-client.done:
		default:
			return client, nil
		}
	}
	cmd := exec.CommandContext(codexRuntime.ctx, config.Cfg.CodexCommand, "app-server", "--enable", "image_generation", "--disable", "shell_tool", "--disable", "multi_agent")
	if config.Cfg.CodexHome != "" {
		cmd.Env = append(os.Environ(), "CODEX_HOME="+config.Cfg.CodexHome)
	}
	cmd.Stderr = log.Writer()
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("无法启动 Codex app-server：%w", err)
	}
	client := &codexClient{cmd: cmd, stdin: stdin, pending: map[string]chan codexRPCMessage{}, listeners: map[string]*codexListener{}, done: make(chan struct{})}
	go client.read(stdout)
	go func() {
		err := cmd.Wait()
		if err == nil {
			err = errors.New("Codex app-server 已退出")
		} else {
			err = fmt.Errorf("Codex app-server 子进程退出：%w", err)
		}
		client.close(err)
	}()
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = client.call(initCtx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "infinite_canvas_go", "title": "Infinite Canvas", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil)
	if err == nil {
		err = client.write(map[string]any{"method": "initialized", "params": map[string]any{}})
	}
	if err != nil {
		client.close(err)
		return nil, err
	}
	codexRuntime.client = client
	return client, nil
}

func (client *codexClient) close(err error) {
	client.once.Do(func() {
		client.err = err
		close(client.done)
		_ = client.stdin.Close()
		if client.cmd.Process != nil {
			_ = client.cmd.Process.Signal(os.Interrupt)
			time.AfterFunc(3*time.Second, func() { _ = client.cmd.Process.Kill() })
		}
	})
}

func (client *codexClient) write(value any) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	select {
	case <-client.done:
		return client.err
	default:
	}
	return json.NewEncoder(client.stdin).Encode(value)
}

func (client *codexClient) call(ctx context.Context, method string, params any, result any, onLateResult ...func(json.RawMessage)) error {
	id := client.nextID.Add(1)
	key := strconv.FormatUint(id, 10)
	reply := make(chan codexRPCMessage, 1)
	client.mu.Lock()
	client.pending[key] = reply
	client.mu.Unlock()
	cleanup := func() { client.mu.Lock(); delete(client.pending, key); client.mu.Unlock() }
	waitingLate := false
	defer func() {
		if !waitingLate {
			cleanup()
		}
	}()
	if err := client.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case response := <-reply:
		if response.Error != nil {
			return fmt.Errorf("Codex app-server %s 失败（错误码 %d）：%s", method, response.Error.Code, response.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(response.Result, result)
		}
		return nil
	case <-ctx.Done():
		if len(onLateResult) > 0 {
			waitingLate = true
			go func() {
				defer cleanup()
				select {
				case response := <-reply:
					if response.Error == nil {
						onLateResult[0](response.Result)
					}
				case <-client.done:
				}
			}()
		}
		return ctx.Err()
	case <-client.done:
		return client.err
	}
}

func (client *codexClient) read(stdout io.Reader) {
	decoder := json.NewDecoder(stdout)
	for {
		var message codexRPCMessage
		if err := decoder.Decode(&message); err != nil {
			client.close(fmt.Errorf("Codex 连接中断：%w", err))
			return
		}
		if message.Method == "" {
			client.mu.Lock()
			reply := client.pending[string(message.ID)]
			client.mu.Unlock()
			if reply != nil {
				select {
				case reply <- message:
				default:
				}
			}
			continue
		}
		if len(message.ID) > 0 {
			var result any
			switch message.Method {
			case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
				result = map[string]string{"decision": "decline"}
			case "item/permissions/requestApproval":
				result = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
			case "item/tool/requestUserInput":
				result = map[string]any{"answers": map[string]any{}}
			case "mcpServer/elicitation/request":
				result = map[string]any{"action": "decline", "content": nil}
			default:
				_ = client.write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": "当前模型执行器不支持此请求"}})
				continue
			}
			_ = client.write(map[string]any{"id": message.ID, "result": result})
			continue
		}
		var params struct {
			ThreadID string `json:"threadId"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			continue
		}
		client.mu.Lock()
		listener := client.listeners[params.ThreadID]
		client.mu.Unlock()
		if listener != nil {
			select {
			case listener.events <- message:
			default:
				listener.fail(errors.New("当前 Codex 任务事件接收过慢，任务已中断，请重试"))
			}
		}
	}
}
