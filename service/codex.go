package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tigerowo/infinite-canvas/config"
	"github.com/tigerowo/infinite-canvas/model"
)

type CodexAIRequest struct {
	Model        string           `json:"model"`
	Prompt       string           `json:"prompt"`
	Instructions string           `json:"instructions"`
	Size         string           `json:"size"`
	Quality      string           `json:"quality"`
	N            int              `json:"n"`
	Stream       bool             `json:"stream"`
	Messages     []map[string]any `json:"messages"`
	Input        any              `json:"input"`
	Tools        []map[string]any `json:"tools"`
	images       [][]byte
}

type CodexImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}
type CodexAIResult struct {
	Text   string
	Images []CodexImage
}
type codexItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Status    string          `json:"status"`
	Phase     string          `json:"phase"`
	Result    string          `json:"result"`
	SavedPath string          `json:"savedPath"`
	Failure   json.RawMessage `json:"failure"`
}
type codexTurnError struct {
	Message           string          `json:"message"`
	AdditionalDetails string          `json:"additionalDetails"`
	Info              json.RawMessage `json:"codexErrorInfo"`
}

func (failure *codexTurnError) Error() string {
	parts := []string{}
	if failure.Message != "" {
		parts = append(parts, failure.Message)
	}
	if len(failure.Info) > 0 && string(failure.Info) != "null" {
		parts = append(parts, "错误类型："+string(failure.Info))
	}
	if failure.AdditionalDetails != "" {
		parts = append(parts, failure.AdditionalDetails)
	}
	if len(parts) == 0 {
		return "Codex 未提供具体错误信息"
	}
	return strings.Join(parts, "\n")
}

type codexTurn struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Items  []codexItem     `json:"items"`
	Error  *codexTurnError `json:"error"`
}

func IsCodexChannel(channel model.ModelChannel) bool {
	return channel.Protocol == ModelChannelProtocolCodex
}

func ReadCodexAIRequest(body []byte, contentType string) (CodexAIRequest, error) {
	var request CodexAIRequest
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		err := json.Unmarshal(body, &request)
		for _, tool := range request.Tools {
			if tool["type"] != "image_generation" {
				continue
			}
			if request.Size == "" {
				request.Size, _ = tool["size"].(string)
			}
			if request.Quality == "" {
				request.Quality, _ = tool["quality"].(string)
			}
		}
		return request, err
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return request, err
	}
	form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(32 << 20)
	if err != nil {
		return request, err
	}
	defer form.RemoveAll()
	value := func(key string) string {
		if values := form.Value[key]; len(values) > 0 {
			return values[0]
		}
		return ""
	}
	request.Model, request.Prompt, request.Size, request.Quality = value("model"), value("prompt"), value("size"), value("quality")
	request.N, _ = strconv.Atoi(value("n"))
	for _, key := range []string{"image", "image[]", "mask"} {
		files := form.File[key]
		for _, file := range files {
			reader, err := file.Open()
			if err != nil {
				return request, err
			}
			data, err := io.ReadAll(io.LimitReader(reader, 20<<20+1))
			_ = reader.Close()
			if err != nil || len(data) > 20<<20 {
				return request, errors.New("参考图片读取失败或超过 20 MB")
			}
			request.images = append(request.images, data)
			if len(request.images) > 16 {
				return request, errors.New("参考图片最多 16 张")
			}
			if key == "mask" {
				request.Prompt += "\n最后一张参考图是编辑蒙版，请按蒙版限定修改区域。"
			}
		}
	}
	return request, nil
}

func CodexImageRequested(channel model.ModelChannel, request CodexAIRequest, path string) bool {
	if strings.HasPrefix(path, "/images/") || request.Model == "codex-image" || channel.ModelCapabilities[request.Model] == "image" {
		return true
	}
	for _, tool := range request.Tools {
		if tool["type"] == "image_generation" {
			return true
		}
	}
	return false
}

func CodexModels(ctx context.Context) ([]string, error) {
	client, err := getCodexClient(ctx)
	if err != nil {
		return nil, err
	}
	var account struct {
		Account      json.RawMessage `json:"account"`
		RequiresAuth bool            `json:"requiresOpenaiAuth"`
	}
	if err := client.call(ctx, "account/read", map[string]bool{"refreshToken": false}, &account); err != nil {
		return nil, err
	}
	if account.RequiresAuth && (len(account.Account) == 0 || string(account.Account) == "null") {
		return nil, errors.New("后端 Codex 未登录，请以运行后端的用户执行 codex login")
	}
	models := []string{"codex-text", "codex-image"}
	cursor := ""
	for {
		var page struct {
			Data []struct {
				Model string `json:"model"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := client.call(ctx, "model/list", params, &page); err != nil {
			return nil, err
		}
		for _, entry := range page.Data {
			models = append(models, entry.Model)
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
	}
	return uniqueModelNames(models), nil
}

func RunCodexAI(ctx context.Context, channel model.ModelChannel, request CodexAIRequest, imageTask bool, onDelta func(string) error) (result CodexAIResult, err error) {
	startedAt := time.Now()
	stage, lastIssue := "启动并连接 app-server", ""
	defer func() {
		if errors.Is(err, context.DeadlineExceeded) {
			message := fmt.Sprintf("Codex 请求超时：已等待 %s，渠道超时 %d 秒；阶段：%s。", time.Since(startedAt).Round(time.Second), maxInt(channel.Timeout, 1), stage)
			if lastIssue != "" {
				message += "\nCodex 最近一次错误：" + lastIssue
			}
			err = errors.New(message)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(maxInt(channel.Timeout, 1))*time.Second)
	defer cancel()
	client, err := getCodexClient(ctx)
	if err != nil {
		return result, err
	}
	root, err := filepath.Abs(config.Cfg.CodexWorkDir)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return result, err
	}
	cwd, err := os.MkdirTemp(root, "task-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(cwd)
	stage = "读取参考图片并准备请求"
	texts := []string{request.Instructions, request.Prompt}
	images := append([][]byte{}, request.images...)
	for _, message := range request.Messages {
		texts = append(texts, fmt.Sprintf("[%v]", message["role"]))
		if err := collectCodexContent(ctx, message["content"], &texts, &images); err != nil {
			return result, err
		}
		if calls, ok := message["tool_calls"]; ok {
			raw, _ := json.Marshal(calls)
			texts = append(texts, string(raw))
		}
	}
	if err := collectCodexContent(ctx, request.Input, &texts, &images); err != nil {
		return result, err
	}
	if len(images) > 16 {
		return result, errors.New("参考图片最多 16 张")
	}
	input := []map[string]any{}
	for index, data := range images {
		if len(data) == 0 || len(data) > 20<<20 || !strings.HasPrefix(http.DetectContentType(data), "image/") {
			return result, errors.New("参考图片内容无效或超过 20 MB")
		}
		path := filepath.Join(cwd, fmt.Sprintf("reference-%d", index+1))
		if err := os.WriteFile(path, data, 0600); err != nil {
			return result, err
		}
		input = append(input, map[string]any{"type": "localImage", "path": path})
	}
	prompt := strings.TrimSpace(strings.Join(texts, "\n\n"))
	if prompt == "" {
		return result, errors.New("请输入提示词")
	}
	instructions := "仅完成文本或参考图片理解任务，不执行命令、不修改文件，不输出进度说明，只返回最终答案。"
	sandbox := "read-only"
	if imageTask {
		sandbox = "workspace-write"
		instructions = fmt.Sprintf("必须调用原生 image_generation 工具生成或编辑真实图片，禁止代码绘图、SVG 或外部 API 替代。图片输出使用 PNG，输出文件保存在当前工作目录内。期望数量 %d、比例 %s、质量 %s；以工具实际能力为准。", maxInt(request.N, 1), request.Size, request.Quality)
	} else if len(request.Tools) > 0 {
		tools, _ := json.Marshal(request.Tools)
		instructions += "\n你不能直接执行下面的工具。需要调用时仅返回 JSON：{\"reply\":\"说明\",\"tool_calls\":[{\"name\":\"工具名\",\"arguments\":{}}]}；不需要调用时返回正常文本。可用工具：" + string(tools)
	}
	threadParams := map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": sandbox, "developerInstructions": instructions, "ephemeral": true}
	actualModel := channel.CodexModel
	if request.Model != "codex-text" && request.Model != "codex-image" {
		actualModel = request.Model
	}
	if actualModel != "" {
		threadParams["model"] = actualModel
	}
	var threadResponse struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	stage = "创建 Codex 线程"
	if err := client.call(ctx, "thread/start", threadParams, &threadResponse); err != nil {
		return result, err
	}
	threadID := threadResponse.Thread.ID
	if threadID == "" {
		return result, errors.New("Codex 未返回线程 ID")
	}
	listener := &codexListener{events: make(chan codexRPCMessage, 256), done: make(chan struct{})}
	client.mu.Lock()
	client.listeners[threadID] = listener
	client.mu.Unlock()
	defer func() { client.mu.Lock(); delete(client.listeners, threadID); client.mu.Unlock() }()
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = client.call(stopCtx, "thread/unsubscribe", map[string]string{"threadId": threadID}, nil)
	}()
	input = append([]map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}}, input...)
	var started struct {
		Turn codexTurn `json:"turn"`
	}
	policy := map[string]any{"type": "readOnly", "networkAccess": false}
	if imageTask {
		policy = map[string]any{"type": "workspaceWrite", "writableRoots": []string{cwd}, "networkAccess": false, "excludeTmpdirEnvVar": true, "excludeSlashTmp": true}
	}
	stage = "提交生成请求"
	err = client.call(ctx, "turn/start", map[string]any{"threadId": threadID, "input": input, "approvalPolicy": "never", "sandboxPolicy": policy}, &started, func(raw json.RawMessage) {
		var response struct { Turn codexTurn `json:"turn"` }
		if json.Unmarshal(raw, &response) == nil {
			interruptCodexTurn(client, threadID, response.Turn.ID)
		}
	})
	if err != nil {
		return result, err
	}
	if started.Turn.ID == "" {
		return result, errors.New("Codex 未返回任务 ID")
	}
	stage = "等待 Codex 生成结果"
	completed := false
	defer func() {
		if !completed {
			interruptCodexTurn(client, threadID, started.Turn.ID)
		}
	}()
	items := map[string]codexItem{}
	itemOrder := []string{}
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-client.done:
			return result, client.err
		case <-listener.done:
			return result, listener.err
		case event := <-listener.events:
			var params struct {
				TurnID    string          `json:"turnId"`
				ItemID    string          `json:"itemId"`
				Delta     string          `json:"delta"`
				Item      codexItem       `json:"item"`
				Turn      codexTurn       `json:"turn"`
				Error     *codexTurnError `json:"error"`
				WillRetry bool            `json:"willRetry"`
			}
			if json.Unmarshal(event.Params, &params) != nil {
				continue
			}
			if params.TurnID != "" && params.TurnID != started.Turn.ID {
				continue
			}
			switch event.Method {
			case "error":
				if params.Error == nil {
					continue
				}
				lastIssue = params.Error.Error()
				if !params.WillRetry {
					return result, fmt.Errorf("Codex 执行失败：%s", lastIssue)
				}
				stage = "Codex 正在重试上游请求"
			case "item/started":
				if params.Item.Type == "imageGeneration" {
					stage = "原生图片工具生成中"
				}
			case "item/agentMessage/delta":
				result.Text += params.Delta
				if onDelta != nil {
					if err := onDelta(params.Delta); err != nil {
						return result, err
					}
				}
			case "item/completed":
				if params.Item.Type == "imageGeneration" {
					if err := codexImageFailure(params.Item); err != nil {
						return result, err
					}
				}
				if _, exists := items[params.Item.ID]; !exists {
					itemOrder = append(itemOrder, params.Item.ID)
				}
				items[params.Item.ID] = params.Item
			case "turn/completed":
				if params.Turn.ID != started.Turn.ID {
					continue
				}
				completed = true
				if params.Turn.Status != "completed" {
					if params.Turn.Error != nil {
						return result, fmt.Errorf("Codex 执行失败：%s", params.Turn.Error.Error())
					}
					message := "Codex 任务未完成：" + params.Turn.Status
					if lastIssue != "" {
						message += "\n" + lastIssue
					}
					return result, errors.New(message)
				}
				for _, item := range params.Turn.Items {
					if _, exists := items[item.ID]; !exists {
						itemOrder = append(itemOrder, item.ID)
					}
					items[item.ID] = item
				}
				if onDelta == nil {
					for _, id := range itemOrder {
						if items[id].Type == "agentMessage" && items[id].Phase == "final_answer" {
							result.Text = items[id].Text
						}
					}
				}
				if result.Text == "" {
					for _, id := range itemOrder {
						if items[id].Type == "agentMessage" {
							result.Text += items[id].Text
						}
					}
					if onDelta != nil && result.Text != "" {
						if err := onDelta(result.Text); err != nil {
							return result, err
						}
					}
				}
				if imageTask {
					stage = "读取并校验图片产物"
					for _, id := range itemOrder {
						item := items[id]
						if item.Type != "imageGeneration" {
							continue
						}
						if err := codexImageFailure(item); err != nil {
							return result, err
						}
						data, err := codexImageBytes(item, cwd)
						if err != nil {
							if result.Text != "" {
								err = fmt.Errorf("%w\nCodex 回复：%s", err, result.Text)
							}
							return result, err
						}
						metadata, format, err := image.DecodeConfig(bytes.NewReader(data))
						if err != nil {
							return result, errors.New("Codex 未返回有效 PNG、JPEG 或 GIF 图片")
						}
						result.Images = append(result.Images, CodexImage{URL: "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(data), Width: metadata.Width, Height: metadata.Height})
					}
					if len(result.Images) == 0 {
						message := "Codex 本轮未返回原生图片产物，请确认当前执行模型和账号支持图片生成"
						if result.Text != "" {
							message += "\nCodex 回复：" + result.Text
						}
						if lastIssue != "" {
							message += "\nCodex 最近一次错误：" + lastIssue
						}
						return result, errors.New(message)
					}
				} else if strings.TrimSpace(result.Text) == "" {
					return result, errors.New("Codex 没有返回文本")
				}
				return result, nil
			}
		}
	}
}

func interruptCodexTurn(client *codexClient, threadID, turnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if turnID == "" {
		return
	}
	if err := client.call(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID}, nil); err != nil {
		log.Printf("Codex 任务中断失败：thread=%s turn=%s err=%v", threadID, turnID, err)
	}
}

func collectCodexContent(ctx context.Context, value any, texts *[]string, images *[][]byte) error {
	switch content := value.(type) {
	case string:
		*texts = append(*texts, content)
	case []any:
		for _, item := range content {
			if err := collectCodexContent(ctx, item, texts, images); err != nil {
				return err
			}
		}
	case map[string]any:
		kind, _ := content["type"].(string)
		if kind == "image_url" || kind == "input_image" {
			url, _ := content["image_url"].(string)
			if imageURL, ok := content["image_url"].(map[string]any); ok {
				url, _ = imageURL["url"].(string)
			}
			data, err := codexReferenceBytes(ctx, url)
			if err != nil {
				return err
			}
			*images = append(*images, data)
			*texts = append(*texts, fmt.Sprintf("[参考图片 %d]", len(*images)))
			return nil
		}
		if role, ok := content["role"].(string); ok {
			*texts = append(*texts, "["+role+"]")
		}
		if text, ok := content["text"].(string); ok {
			*texts = append(*texts, text)
		}
		if output, ok := content["output"].(string); ok {
			*texts = append(*texts, output)
		}
		if kind == "function_call" {
			raw, _ := json.Marshal(content)
			*texts = append(*texts, string(raw))
		}
		if nested, ok := content["content"]; ok {
			return collectCodexContent(ctx, nested, texts, images)
		}
	}
	return nil
}

func codexReferenceBytes(ctx context.Context, url string) ([]byte, error) {
	if strings.HasPrefix(url, "data:image/") {
		_, encoded, ok := strings.Cut(url, ",")
		if !ok || len(encoded) > 28<<20 {
			return nil, errors.New("参考图片格式无效或过大")
		}
		return base64.StdEncoding.DecodeString(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil || (request.URL.Scheme != "https" && request.URL.Scheme != "http") {
		return nil, errors.New("参考图片地址无效")
	}
	response, err := SafeProxyHTTPClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("参考图片下载失败")
	}
	return io.ReadAll(io.LimitReader(response.Body, 20<<20+1))
}

func codexImageBytes(item codexItem, cwd string) ([]byte, error) {
	if item.Result != "" {
		encoded := item.Result
		if strings.HasPrefix(encoded, "data:image/") {
			_, encoded, _ = strings.Cut(encoded, ",")
		}
		if len(encoded) > 28<<20 {
			return nil, errors.New("Codex 图片超过 20 MB")
		}
		if data, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(data) > 0 {
			if len(data) > 20<<20 {
				return nil, errors.New("Codex 图片超过 20 MB")
			}
			return data, nil
		}
	}
	if item.SavedPath == "" {
		return nil, fmt.Errorf("Codex 图片工具未返回有效图片数据或输出路径（状态 %s）", item.Status)
	}
	path, err := filepath.EvalSymlinks(item.SavedPath)
	if err != nil {
		return nil, fmt.Errorf("Codex 图片产物不可读取（状态 %s）：%w", item.Status, err)
	}
	base, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return nil, errors.New("Codex 图片产物不在当前任务目录内")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 20<<20+1))
	if len(data) > 20<<20 {
		return nil, errors.New("Codex 图片超过 20 MB")
	}
	return data, err
}

func ExecuteCodexAI(ctx context.Context, user model.AuthUser, channel model.ModelChannel, request CodexAIRequest, path string, credits float64, onDelta func(string) error) (CodexAIResult, error) {
	started := time.Now()
	if credits > 0 {
		if err := ConsumeUserCredits(user.ID, request.Model, credits, path); err != nil {
			return CodexAIResult{}, err
		}
	}
	result, err := RunCodexAI(ctx, channel, request, CodexImageRequested(channel, request, path), onDelta)
	if err == nil && !CodexImageRequested(channel, request, path) {
		_, err = CodexChatMessage(result.Text, request.Tools)
	}
	status, failure := http.StatusOK, ""
	if err != nil {
		status, failure = http.StatusBadGateway, err.Error()
		if credits > 0 && ctx.Err() == nil {
			_ = RefundUserCredits(user.ID, request.Model, credits, path)
		}
	}
	requestLog, _ := json.Marshal(map[string]any{"model": request.Model, "prompt": request.Prompt, "imageInputs": len(request.images)})
	responseLog := result.Text
	if len(result.Images) > 0 {
		responseLog = fmt.Sprintf("Codex 生成 %d 张图片", len(result.Images))
	}
	SaveAICallLog(AICallLogInput{UserID: user.ID, UserDisplayName: user.DisplayName, Endpoint: path, Method: "POST", Model: request.Model, ChannelID: channel.ID, ChannelName: channel.Name, Credits: credits, Status: status, DurationMs: time.Since(started).Milliseconds(), RequestBody: string(requestLog), ResponseBody: responseLog, Error: failure})
	return result, err
}

func CodexChatMessage(text string, tools []map[string]any) (map[string]any, error) {
	message := map[string]any{"role": "assistant", "content": text}
	if len(tools) == 0 {
		return message, nil
	}
	var parsed struct {
		Reply string `json:"reply"`
		Calls []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"tool_calls"`
	}
	cleaned := strings.TrimSpace(text)
	if strings.HasPrefix(cleaned, "```") {
		_, cleaned, _ = strings.Cut(cleaned, "\n")
		cleaned = strings.TrimSpace(strings.TrimSuffix(cleaned, "```"))
	}
	if json.Unmarshal([]byte(cleaned), &parsed) != nil || len(parsed.Calls) == 0 {
		return message, nil
	}
	allowed := map[string]bool{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if function, ok := tool["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
		}
		allowed[name] = true
	}
	calls := []map[string]any{}
	for _, call := range parsed.Calls {
		if !allowed[call.Name] || call.Arguments == nil {
			return nil, errors.New("Codex 返回了无效工具调用")
		}
		arguments, _ := json.Marshal(call.Arguments)
		calls = append(calls, map[string]any{"id": "call_" + uuid.NewString(), "type": "function", "function": map[string]any{"name": call.Name, "arguments": string(arguments)}})
	}
	message["content"], message["tool_calls"] = parsed.Reply, calls
	return message, nil
}

func normalizeCodexChannel(channel model.ModelChannel) model.ModelChannel {
	if !IsCodexChannel(channel) {
		return channel
	}
	channel.BaseURL, channel.APIKey, channel.ParameterTranslation = "", "", ""
	channel.CodexModel = strings.TrimSpace(channel.CodexModel)
	if len(channel.Models) == 0 {
		channel.Models = []string{"codex-text", "codex-image"}
	}
	if channel.ModelCapabilities == nil {
		channel.ModelCapabilities = map[string]string{}
	}
	for _, name := range channel.Models {
		if name == "codex-image" {
			channel.ModelCapabilities[name] = "image"
		} else if channel.ModelCapabilities[name] == "" {
			channel.ModelCapabilities[name] = "text"
		}
	}
	return channel
}

func testCodexChannelModel(channel model.ModelChannel, modelName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(channel.Timeout)*time.Second)
	defer cancel()
	if _, err := CodexModels(ctx); err != nil {
		return "", err
	}
	if modelName == "codex-image" || channel.ModelCapabilities[modelName] == "image" {
		return "Codex 已登录且 app-server 可连接；请在画布生成图片验证账号的原生生图能力。", nil
	}
	result, err := RunCodexAI(ctx, channel, CodexAIRequest{Model: modelName, Prompt: "仅回复 OK"}, false, nil)
	return result.Text, err
}

func codexImageFailure(item codexItem) error {
	if len(item.Failure) == 0 || string(item.Failure) == "null" {
		return nil
	}
	var failure struct {
		Type     string `json:"type"`
		LimitID  string `json:"limitId"`
		ResetsAt *int64 `json:"resetsAt"`
	}
	if json.Unmarshal(item.Failure, &failure) == nil && failure.Type == "usageLimitExceeded" {
		message := "Codex 原生图片生成额度已用完"
		if failure.LimitID != "" {
			message += "（额度类型：" + failure.LimitID + "）"
		}
		if failure.ResetsAt != nil {
			message += "；恢复时间：" + time.Unix(*failure.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		return errors.New(message)
	}
	return fmt.Errorf("Codex 图片工具失败（状态 %s）：%s", item.Status, item.Failure)
}
