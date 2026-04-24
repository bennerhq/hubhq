package script

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type JavaScriptEngine struct{}

func (JavaScriptEngine) Validate(doc Document) error {
	if err := ValidateDocument(doc); err != nil {
		return err
	}
	if normalizedLanguage(doc.Language) != LanguageJavaScript {
		return fmt.Errorf("javascript engine cannot validate language %q", doc.Language)
	}
	for _, blocked := range []string{"require(", "import ", "fetch(", "XMLHttpRequest", "child_process", "fs.", "process.env", "Deno.", "Bun."} {
		if strings.Contains(doc.Source, blocked) {
			return fmt.Errorf("javascript source uses unsupported feature %q", blocked)
		}
	}
	return nil
}

func (JavaScriptEngine) Execute(ctx context.Context, execCtx ExecContext, doc Document) (*Result, error) {
	if err := ValidateDocument(doc); err != nil {
		return nil, err
	}
	path, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("node runtime not found: %w", err)
	}
	var devices []map[string]any
	var scenes []map[string]any
	var speakers []map[string]any
	state := map[string]any{}
	if execCtx.DevicesSnapshot != nil {
		devices = execCtx.DevicesSnapshot
	} else if execCtx.Devices != nil {
		devices, _ = execCtx.Devices.ListDevices(ctx)
	}
	if execCtx.ScenesSnapshot != nil {
		scenes = execCtx.ScenesSnapshot
	} else if execCtx.Devices != nil {
		scenes, _ = execCtx.Devices.ListScenes(ctx)
	}
	if execCtx.SpeakersSnapshot != nil {
		speakers = execCtx.SpeakersSnapshot
	} else if execCtx.Speakers != nil {
		speakers, _ = execCtx.Speakers.ListSpeakers(ctx)
	}
	if execCtx.State != nil {
		state, _ = execCtx.State.Snapshot()
	}
	now := time.Now
	if execCtx.Now != nil {
		now = execCtx.Now
	}
	wrapper, err := buildNodeWrapper(doc, devices, scenes, speakers, state, now(), execCtx.ScriptInput, execCtx.ScriptArgs, execCtx.StdinText)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, path, "-e", wrapper)
	var stderr bytes.Buffer
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	result := &Result{}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, protocolPrefix) {
			if strings.TrimSpace(line) != "" {
				result.Logs = append(result.Logs, line)
			}
			continue
		}
		msg := strings.TrimPrefix(line, protocolPrefix)
		if err := handleProtocolMessage(ctx, execCtx, result, stdin, msg); err != nil {
			_ = stdin.Close()
			_ = cmd.Wait()
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, err
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("javascript runtime failed: %s", strings.TrimSpace(stderr.String()))
		}
		return nil, err
	}
	return result, nil
}

const protocolPrefix = "__DIRIGERA__"

type protocolMessage struct {
	Type           string            `json:"type"`
	Op             string            `json:"op,omitempty"`
	RequestID      string            `json:"request_id,omitempty"`
	ID             string            `json:"id,omitempty"`
	Name           string            `json:"name,omitempty"`
	Sound          string            `json:"sound,omitempty"`
	Method         string            `json:"method,omitempty"`
	URL            string            `json:"url,omitempty"`
	Room           string            `json:"room,omitempty"`
	Scope          string            `json:"scope,omitempty"`
	Path           string            `json:"path,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Key            string            `json:"key,omitempty"`
	Attrs          map[string]any    `json:"attrs,omitempty"`
	Message        string            `json:"message,omitempty"`
	Value          any               `json:"value,omitempty"`
	Error          string            `json:"error,omitempty"`
}

func buildNodeWrapper(doc Document, devices, scenes, speakers []map[string]any, state map[string]any, now time.Time, input any, args []string, stdinText string) (string, error) {
	source := base64.StdEncoding.EncodeToString([]byte(doc.Source))
	entry, err := json.Marshal(firstNonEmpty(doc.EntryPoint, "main"))
	if err != nil {
		return "", err
	}
	devicesJSON, err := json.Marshal(devices)
	if err != nil {
		return "", err
	}
	scenesJSON, err := json.Marshal(scenes)
	if err != nil {
		return "", err
	}
	speakersJSON, err := json.Marshal(speakers)
	if err != nil {
		return "", err
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	nowJSON, err := json.Marshal(now.UTC().Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	stdinJSON, err := json.Marshal(stdinText)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`const entryPoint = %s;
const source = Buffer.from(%q, "base64").toString("utf8");
const devices = %s;
const scenes = %s;
const speakers = %s;
const state = %s;
const nowIso = %s;
const scriptInput = %s;
const scriptArgs = %s;
const stdinText = %s;
const emit = (payload) => process.stdout.write(%q + JSON.stringify(payload) + "\n");
const normalize = (value) => String(value || "").trim().toLowerCase();
let request = async () => { throw new Error("request bridge not initialized"); };
const roomNameOf = (device) => {
  if (!device) return "";
  if (typeof device.room === "string") return device.room;
  if (device.room && typeof device.room.name === "string") return device.room.name;
  return "";
};
const runtimeDevices = devices.map((device) => {
  if (!device || device.is_reachable !== false) return device;
  const nowMillis = Date.parse(nowIso);
  const lastSeenMillis = Date.parse(String(device.last_seen || ""));
  const staleMillis = 10 * 60 * 1000;
  if (!Number.isNaN(lastSeenMillis) && !Number.isNaN(nowMillis) && (nowMillis - lastSeenMillis) <= staleMillis) {
    return device;
  }
  const attrs = Object.assign({}, device.attributes || {});
  if (Object.prototype.hasOwnProperty.call(attrs, "isOn")) attrs.isOn = null;
  return Object.assign({}, device, { attributes: attrs });
});
const api = {
  log: (...args) => emit({ type: "log", message: args.map((arg) => typeof arg === "string" ? arg : JSON.stringify(arg)).join(" ") }),
  logJson: (value) => emit({ type: "log", message: JSON.stringify(value) }),
  devices: {
    all: () => runtimeDevices,
    listByRoom: (roomName) => runtimeDevices.filter((device) => normalize(roomNameOf(device)) === normalize(roomName)),
    listByType: (type) => runtimeDevices.filter((device) => normalize(device.type || device.deviceType) === normalize(type)),
    listByName: (name) => runtimeDevices.filter((device) => normalize(device.name) === normalize(name) || normalize(device.custom_name) === normalize(name)),
    getLiveByID: async (id) => await request("devices.getLiveByID", { id }),
    getLiveByName: async (name) => await request("devices.getLiveByName", { name }),
    listLiveByRoom: async (roomName) => await request("devices.listLiveByRoom", { room: roomName }),
    listLiveByType: async (type) => await request("devices.listLiveByType", { value: type }),
    listLiveByName: async (name) => await request("devices.listLiveByName", { name }),
    setAttributes: async (id, attrs) => {
      emit({ type: "op", op: "devices.setAttributes", id, attrs });
      return { ok: true };
    },
    setAttributesByName: async (name, attrs) => {
      emit({ type: "op", op: "devices.setAttributesByName", name, attrs });
      return { ok: true };
    },
    setRoomAttributes: async (roomName, attrs) => {
      emit({ type: "op", op: "devices.setRoomAttributes", room: roomName, attrs });
      return { ok: true };
    },
    isOpen: (device) => {
      const attrs = (device && device.attributes) || {};
      if (attrs.isOpen !== undefined) return attrs.isOpen === true;
      if (attrs.windowOpen !== undefined) return attrs.windowOpen === true;
      if (attrs.open !== undefined) return attrs.open === true;
      if (attrs.contact !== undefined) return attrs.contact === true;
      if (attrs.contactState !== undefined) return normalize(attrs.contactState) === "open";
      return false;
    },
    isClosed: (device) => {
      const attrs = (device && device.attributes) || {};
      if (attrs.isOpen !== undefined) return attrs.isOpen === false;
      if (attrs.windowOpen !== undefined) return attrs.windowOpen === false;
      if (attrs.open !== undefined) return attrs.open === false;
      if (attrs.contact !== undefined) return attrs.contact === false;
      if (attrs.contactState !== undefined) return normalize(attrs.contactState) === "closed";
      return false;
    },
  },
  scenes: {
    all: () => scenes,
    trigger: async (id) => {
      emit({ type: "op", op: "scenes.trigger", id });
      return { ok: true };
    },
    triggerByName: async (name) => {
      emit({ type: "op", op: "scenes.triggerByName", name });
      return { ok: true };
    },
  },
  speakers: {
    all: () => speakers,
    playSound: async (id, sound) => {
      emit({ type: "op", op: "speakers.playSound", id, sound });
      return { ok: true };
    },
    playSoundByName: async (name, sound) => {
      emit({ type: "op", op: "speakers.playSoundByName", name, sound });
      return { ok: true };
    },
    playTest: async (id) => {
      emit({ type: "op", op: "speakers.playSound", id, sound: "sound-0" });
      return { ok: true };
    },
    playTestByName: async (name) => {
      emit({ type: "op", op: "speakers.playSoundByName", name, sound: "sound-0" });
      return { ok: true };
    },
  },
  state: {
    get: (key) => state[key],
    set: async (key, value) => {
      state[key] = value;
      emit({ type: "op", op: "state.set", key, value });
      return value;
    },
    delete: async (key) => {
      delete state[key];
      emit({ type: "op", op: "state.delete", key });
      return true;
    },
  },
  time: {
    now: () => nowIso,
  },
  guard: {
    debounce: async (key, seconds) => {
      const current = Date.parse(nowIso);
      const previous = Date.parse(String(state[key] || ""));
      if (!Number.isNaN(previous) && ((current - previous) / 1000) < seconds) {
        return false;
      }
      state[key] = nowIso;
      emit({ type: "op", op: "state.set", key, value: nowIso });
      return true;
    },
    throttle: async (key, seconds) => {
      const current = Date.parse(nowIso);
      const previous = Date.parse(String(state[key] || ""));
      if (!Number.isNaN(previous) && ((current - previous) / 1000) < seconds) {
        return false;
      }
      state[key] = nowIso;
      emit({ type: "op", op: "state.set", key, value: nowIso });
      return true;
    },
  },
  access: {
    root: async () => await request("access.root"),
    list: async (scope, path = ".") => await request("access.list", { scope, path }),
    readText: async (scope, path) => await request("access.readText", { scope, path }),
    writeText: async (scope, path, value) => await request("access.writeText", { scope, path, value }),
    readJson: async (scope, path) => await request("access.readJson", { scope, path }),
    writeJson: async (scope, path, value) => await request("access.writeJson", { scope, path, value }),
    delete: async (scope, path) => await request("access.delete", { scope, path }),
    listScripts: async () => await request("access.listScripts"),
    runScript: async (name, args = [], input = null) => await request("access.runScript", { name, args, value: input }),
  },
  web: {
    request: async (method, url, options = {}) => {
      const payload = {
        method: String(method || "GET"),
        url: String(url || ""),
        headers: (options && options.headers) || {},
      };
      if (options && Object.prototype.hasOwnProperty.call(options, "body")) {
        payload.value = options.body;
      }
      return await request("http.do", payload);
    },
    get: async (url, headers = {}) => await request("http.do", { method: "GET", url: String(url || ""), headers }),
    post: async (url, body = null, headers = {}) => await request("http.do", { method: "POST", url: String(url || ""), headers, value: body }),
  },
  tools: {
    run: async (name, options = {}) => await request("tools.run", {
      name: String(name || ""),
      args: Array.isArray(options.args) ? options.args : [],
      value: Object.prototype.hasOwnProperty.call(options, "stdin") ? String(options.stdin ?? "") : "",
      timeout_seconds: Number(options.timeout_seconds || 30),
    }),
  },
  script: {
    input: () => scriptInput,
    args: () => scriptArgs.slice(),
  },
  stdin: {
    readText: () => stdinText,
    readJson: () => JSON.parse(stdinText || "null"),
  },
  stdout: {
    write: (value) => emit({ type: "stdout", message: String(value ?? "") }),
    writeLine: (value = "") => emit({ type: "stdout", message: String(value ?? "") + "\n" }),
    writeJson: (value) => emit({ type: "stdout", message: JSON.stringify(value) + "\n" }),
  },
  stderr: {
    write: (value) => emit({ type: "stderr", message: String(value ?? "") }),
    writeLine: (value = "") => emit({ type: "stderr", message: String(value ?? "") + "\n" }),
    writeJson: (value) => emit({ type: "stderr", message: JSON.stringify(value) + "\n" }),
  },
};
(function setupProtocolResponses() {
  const readline = require("node:readline");
  const pending = new Map();
  let requestSeq = 0;
  request = (op, payload = {}) => new Promise((resolve, reject) => {
    const requestId = "req_" + (++requestSeq);
    pending.set(requestId, { resolve, reject });
    emit(Object.assign({ type: "request", request_id: requestId, op }, payload));
  });
  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  globalThis.__hubhqCloseProtocol = () => rl.close();
  rl.on("line", (line) => {
    if (!line) return;
    let payload;
    try {
      payload = JSON.parse(line);
    } catch (_) {
      return;
    }
    if (!payload || payload.type !== "response" || !payload.request_id) return;
    const pendingRequest = pending.get(payload.request_id);
    if (!pendingRequest) return;
    pending.delete(payload.request_id);
    if (payload.error) {
      pendingRequest.reject(new Error(String(payload.error)));
      return;
    }
    pendingRequest.resolve(payload.value);
  });
})();
(async () => {
  try {
    const loadEntryPoint = new Function("entryPoint", source + "\ntry { return eval(entryPoint); } catch (_) { return globalThis[entryPoint]; }");
    const entry = loadEntryPoint(entryPoint);
    if (typeof entry !== "function") {
      throw new Error("entry point not found: " + entryPoint);
    }
    const value = await entry(api, scriptInput, scriptArgs);
    if (typeof globalThis.__hubhqCloseProtocol === "function") {
      globalThis.__hubhqCloseProtocol();
    }
    emit({ type: "result", value });
  } catch (err) {
    if (typeof globalThis.__hubhqCloseProtocol === "function") {
      globalThis.__hubhqCloseProtocol();
    }
    const message = err && err.stack ? err.stack : String(err);
    process.stderr.write(message + "\n");
    process.exit(1);
  }
})();
`, string(entry), source, string(devicesJSON), string(scenesJSON), string(speakersJSON), string(stateJSON), string(nowJSON), string(inputJSON), string(argsJSON), string(stdinJSON), protocolPrefix), nil
}

func handleProtocolMessage(ctx context.Context, execCtx ExecContext, result *Result, stdin io.Writer, raw string) error {
	var msg protocolMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return fmt.Errorf("decode javascript protocol message: %w", err)
	}
	switch msg.Type {
	case "log":
		if strings.TrimSpace(msg.Message) != "" {
			result.Logs = append(result.Logs, msg.Message)
			if execCtx.Logger != nil {
				execCtx.Logger.Printf("%s", msg.Message)
			}
		}
		return nil
	case "stdout":
		if execCtx.Stdout != nil && msg.Message != "" {
			if _, err := io.WriteString(execCtx.Stdout, msg.Message); err != nil {
				return err
			}
		}
		return nil
	case "stderr":
		if execCtx.Stderr != nil && msg.Message != "" {
			if _, err := io.WriteString(execCtx.Stderr, msg.Message); err != nil {
				return err
			}
		}
		return nil
	case "result":
		result.Output = msg.Value
		return nil
	case "request":
		response, err := handleProtocolRequest(ctx, execCtx, msg)
		if err != nil {
			response = protocolMessage{
				Type:      "response",
				RequestID: msg.RequestID,
				Error:     err.Error(),
			}
		}
		return writeProtocolMessage(stdin, response)
	case "op":
		switch msg.Op {
		case "devices.setAttributes":
			if execCtx.Devices == nil {
				return fmt.Errorf("devices service is not configured")
			}
			if err := execCtx.Devices.SetAttributes(ctx, msg.ID, msg.Attrs); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "devices.setAttributesByName":
			if execCtx.Devices == nil {
				return fmt.Errorf("devices service is not configured")
			}
			if err := execCtx.Devices.SetAttributesByName(ctx, msg.Name, msg.Attrs); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "devices.setRoomAttributes":
			if execCtx.Devices == nil {
				return fmt.Errorf("devices service is not configured")
			}
			if err := execCtx.Devices.SetRoomAttributes(ctx, msg.Room, msg.Attrs); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "scenes.trigger":
			if execCtx.Devices == nil {
				return fmt.Errorf("devices service is not configured")
			}
			if err := execCtx.Devices.TriggerScene(ctx, msg.ID); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "scenes.triggerByName":
			if execCtx.Devices == nil {
				return fmt.Errorf("devices service is not configured")
			}
			if err := execCtx.Devices.TriggerSceneByName(ctx, msg.Name); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "speakers.playTest":
			if execCtx.Speakers == nil {
				return fmt.Errorf("speaker service is not configured")
			}
			if err := execCtx.Speakers.PlayTestByID(ctx, msg.ID); err != nil {
				return err
			}
			return nil
		case "speakers.playTestByName":
			if execCtx.Speakers == nil {
				return fmt.Errorf("speaker service is not configured")
			}
			if err := execCtx.Speakers.PlayTestByName(ctx, msg.Name); err != nil {
				return err
			}
			return nil
		case "speakers.playSound":
			if execCtx.Speakers == nil {
				return fmt.Errorf("speaker service is not configured")
			}
			if err := execCtx.Speakers.PlaySoundByID(ctx, msg.ID, msg.Sound); err != nil {
				return err
			}
			return nil
		case "speakers.playSoundByName":
			if execCtx.Speakers == nil {
				return fmt.Errorf("speaker service is not configured")
			}
			if err := execCtx.Speakers.PlaySoundByName(ctx, msg.Name, msg.Sound); err != nil {
				return err
			}
			return nil
		case "state.set":
			if execCtx.State == nil {
				return fmt.Errorf("state store is not configured")
			}
			if err := execCtx.State.Set(msg.Key, msg.Value); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "state.delete":
			if execCtx.State == nil {
				return fmt.Errorf("state store is not configured")
			}
			if err := execCtx.State.Delete(msg.Key); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "access.writeText":
			if execCtx.Access == nil {
				return fmt.Errorf("access service is not configured")
			}
			if err := execCtx.Access.WriteText(ctx, msg.Scope, msg.Path, fmt.Sprint(msg.Value)); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "access.writeJson":
			if execCtx.Access == nil {
				return fmt.Errorf("access service is not configured")
			}
			if err := execCtx.Access.WriteJSON(ctx, msg.Scope, msg.Path, msg.Value); err != nil {
				return err
			}
			result.Updated = true
			return nil
		case "access.delete":
			if execCtx.Access == nil {
				return fmt.Errorf("access service is not configured")
			}
			if err := execCtx.Access.Delete(ctx, msg.Scope, msg.Path); err != nil {
				return err
			}
			result.Updated = true
			return nil
		default:
			return fmt.Errorf("unsupported javascript operation %q", msg.Op)
		}
	default:
		return fmt.Errorf("unsupported javascript message type %q", msg.Type)
	}
}

func handleProtocolRequest(ctx context.Context, execCtx ExecContext, msg protocolMessage) (protocolMessage, error) {
	response := protocolMessage{
		Type:      "response",
		RequestID: msg.RequestID,
	}
	switch msg.Op {
	case "devices.getLiveByID":
		if execCtx.Devices == nil {
			return response, fmt.Errorf("devices service is not configured")
		}
		item, err := execCtx.Devices.GetLiveDeviceByID(ctx, msg.ID)
		if err != nil {
			return response, err
		}
		response.Value = item
		return response, nil
	case "devices.getLiveByName":
		if execCtx.Devices == nil {
			return response, fmt.Errorf("devices service is not configured")
		}
		item, err := execCtx.Devices.GetLiveDeviceByName(ctx, msg.Name)
		if err != nil {
			return response, err
		}
		response.Value = item
		return response, nil
	case "devices.listLiveByRoom":
		if execCtx.Devices == nil {
			return response, fmt.Errorf("devices service is not configured")
		}
		items, err := execCtx.Devices.ListLiveDevicesByRoom(ctx, msg.Room)
		if err != nil {
			return response, err
		}
		response.Value = items
		return response, nil
	case "devices.listLiveByType":
		if execCtx.Devices == nil {
			return response, fmt.Errorf("devices service is not configured")
		}
		items, err := execCtx.Devices.ListLiveDevicesByType(ctx, fmt.Sprint(msg.Value))
		if err != nil {
			return response, err
		}
		response.Value = items
		return response, nil
	case "devices.listLiveByName":
		if execCtx.Devices == nil {
			return response, fmt.Errorf("devices service is not configured")
		}
		items, err := execCtx.Devices.ListLiveDevicesByName(ctx, msg.Name)
		if err != nil {
			return response, err
		}
		response.Value = items
		return response, nil
	case "access.root":
		if execCtx.Access == nil {
			return response, fmt.Errorf("access service is not configured")
		}
		value, err := execCtx.Access.Root(ctx)
		if err != nil {
			return response, err
		}
		response.Value = value
		return response, nil
	case "access.list":
		if execCtx.Access == nil {
			return response, fmt.Errorf("access service is not configured")
		}
		value, err := execCtx.Access.List(ctx, msg.Scope, msg.Path)
		if err != nil {
			return response, err
		}
		response.Value = value
		return response, nil
	case "access.readText":
		if execCtx.Access == nil {
			return response, fmt.Errorf("access service is not configured")
		}
		value, err := execCtx.Access.ReadText(ctx, msg.Scope, msg.Path)
		if err != nil {
			return response, err
		}
		response.Value = value
		return response, nil
	case "access.readJson":
		if execCtx.Access == nil {
			return response, fmt.Errorf("access service is not configured")
		}
		value, err := execCtx.Access.ReadJSON(ctx, msg.Scope, msg.Path)
		if err != nil {
			return response, err
		}
		response.Value = value
		return response, nil
	case "access.listScripts":
		if execCtx.Access == nil {
			return response, fmt.Errorf("access service is not configured")
		}
		value, err := execCtx.Access.ListScripts(ctx)
		if err != nil {
			return response, err
		}
		response.Value = value
		return response, nil
	case "access.runScript":
		if execCtx.Access == nil {
			return response, fmt.Errorf("access service is not configured")
		}
		value, err := execCtx.Access.RunScript(ctx, msg.Name, msg.Args, msg.Value)
		if err != nil {
			return response, err
		}
		response.Value = value
		return response, nil
	case "http.do":
		if execCtx.HTTP == nil {
			return response, fmt.Errorf("http client is not configured")
		}
		value, err := execCtx.HTTP.Do(ctx, msg.Method, msg.URL, msg.Value, msg.Headers)
		if err != nil {
			return response, err
		}
		response.Value = value
		return response, nil
	case "tools.run":
		if execCtx.Tools == nil {
			return response, fmt.Errorf("tool service is not configured")
		}
		value, err := execCtx.Tools.Run(ctx, msg.Name, msg.Args, fmt.Sprint(msg.Value), msg.TimeoutSeconds)
		if err != nil {
			return response, err
		}
		response.Value = value
		return response, nil
	default:
		return response, fmt.Errorf("unsupported javascript request %q", msg.Op)
	}
}

func writeProtocolMessage(w io.Writer, msg protocolMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		if errors.Is(err, syscall.EPIPE) {
			return nil
		}
		return err
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
