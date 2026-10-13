package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tigerowo/infinite-canvas/model"
	"github.com/tigerowo/infinite-canvas/service"
)

func serveCodexAI(w http.ResponseWriter, r *http.Request, body []byte, contentType, path string, channel model.ModelChannel, user model.AuthUser, credits float64) {
	if path != "/chat/completions" && path != "/responses" && path != "/images/generations" && path != "/images/edits" {
		Fail(w, "Codex 当前只支持文本、图片理解和图片生成")
		return
	}
	request, err := service.ReadCodexAIRequest(body, contentType)
	if err != nil {
		Fail(w, "Codex 请求参数无效")
		return
	}
	imageTask := service.CodexImageRequested(channel, request, path)
	stream := request.Stream && !imageTask
	startedStream := false
	id := "codex_" + uuid.NewString()
	send := func(event string, payload any) error {
		if !startedStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			startedStream = true
		}
		if event != "" {
			if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
				return err
			}
		}
		data, _ := json.Marshal(payload)
		_, err := fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return err
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	var onDelta func(string) error
	if stream && len(request.Tools) == 0 {
		onDelta = func(delta string) error {
			if path == "/responses" {
				return send("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": delta})
			}
			return send("", chunk(map[string]any{"content": delta}, nil))
		}
	}
	result, err := service.ExecuteCodexAI(r.Context(), user, channel, request, path, credits, onDelta)
	if err != nil {
		if startedStream {
			_ = send("error", map[string]any{"error": map[string]string{"message": err.Error()}})
		} else {
			Fail(w, err.Error())
		}
		return
	}
	if imageTask {
		w.Header().Set("Content-Type", "application/json")
		if path == "/responses" {
			output := []any{}
			for _, image := range result.Images {
				_, encoded, _ := strings.Cut(image.URL, ",")
				output = append(output, map[string]any{"id": uuid.NewString(), "type": "image_generation_call", "status": "completed", "result": encoded})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "response", "status": "completed", "output": output})
		} else if path == "/chat/completions" {
			images := []any{}
			for _, image := range result.Images {
				images = append(images, map[string]any{"image_url": map[string]string{"url": image.URL}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "", "images": images}}}})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"created": time.Now().Unix(), "data": result.Images})
		}
		return
	}
	message, err := service.CodexChatMessage(result.Text, request.Tools)
	if err != nil {
		if startedStream {
			_ = send("error", map[string]any{"error": map[string]string{"message": err.Error()}})
		} else {
			Fail(w, err.Error())
		}
		return
	}
	finish := "stop"
	if _, ok := message["tool_calls"]; ok {
		finish = "tool_calls"
	}
	if path == "/responses" {
		output := []any{map[string]any{"id": id + "_message", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": message["content"]}}}}
		if calls, ok := message["tool_calls"].([]map[string]any); ok {
			for _, call := range calls {
				function := call["function"].(map[string]any)
				output = append(output, map[string]any{"type": "function_call", "call_id": call["id"], "name": function["name"], "arguments": function["arguments"]})
			}
		}
		response := map[string]any{"id": id, "object": "response", "status": "completed", "output_text": message["content"], "output": output}
		if stream {
			_ = send("response.completed", map[string]any{"type": "response.completed", "response": response})
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
		return
	}
	if stream {
		if len(request.Tools) > 0 {
			delta := map[string]any{"content": message["content"]}
			if calls, ok := message["tool_calls"].([]map[string]any); ok {
				for index := range calls {
					calls[index]["index"] = index
				}
				delta["tool_calls"] = calls
			}
			_ = send("", chunk(delta, nil))
		}
		_ = send("", chunk(map[string]any{}, finish))
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "chat.completion", "model": request.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
	}
}
