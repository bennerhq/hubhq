package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jbenner/hubhq/internal/autos"
	"github.com/jbenner/hubhq/internal/config"
	"github.com/jbenner/hubhq/internal/script"
)

type OpenAICompatibleProvider struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
	cache   *diskCache
	debugf  func(string, ...any)
}

func NewOpenAICompatibleProvider(cfg config.LLMConfig) *OpenAICompatibleProvider {
	timeout := 45 * time.Second
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return &OpenAICompatibleProvider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		client:  &http.Client{Timeout: timeout},
		cache:   newDiskCache(cfg),
	}
}

func (p *OpenAICompatibleProvider) SetDebugLogger(debugf func(string, ...any)) {
	p.debugf = debugf
}

func (p *OpenAICompatibleProvider) GenerateActionPlan(ctx context.Context, prompt string, home HomeContext) (*ActionPlan, error) {
	schema := `{
  "mode": "answer | execute",
  "summary": "short summary",
  "requires_confirmation": false,
  "destructive": false,
  "actions": [
    {
      "type": "set_device_attributes | trigger_scene | rename_device | rename_scene | delete_device | delete_scene | set_speaker_volume | set_speaker_playback",
      "target_id": "device-or-scene-id",
      "target_name": "human name",
      "attributes": {"isOn": true, "lightLevel": 50},
      "value": "for speaker playback values like playbackPlaying or playbackPaused"
    }
  ]
}`
	system := "You are an IKEA DIRIGERA CLI planner. Return JSON only. Use the supplied live home context. Users may reference devices by any value shown in the devices list columns NAME, ROOM, TYPE, or ID, including combinations like room plus type or room plus name. Match against any of those fields from the supplied device context and prefer exact target_id values from context in the final actions. For informational questions, return mode=answer with no actions. For destructive actions set destructive=true and requires_confirmation=true. Do not invent devices or scenes. Speaker control is allowed only for live speaker devices in context. Use set_speaker_volume with attributes.volume or set_speaker_playback with value=playbackPlaying|playbackPaused|playbackNext|playbackPrevious."
	user := buildPrompt("Create an action plan", prompt, home, schema)

	var plan ActionPlan
	if err := p.completeJSON(ctx, system, user, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (p *OpenAICompatibleProvider) GenerateAutomationSpec(ctx context.Context, prompt string, home HomeContext) (*autos.Spec, error) {
	schema := `{
  "summary": "short summary",
  "trigger": {
    "kind": "time | sun_event | manual | event",
    "at": "HH:MM",
    "sun_event": "sunrise | sunset",
    "offset": "+00:15",
    "cron": "optional cron",
    "time_zone": "IANA tz",
    "device_id": "device id for event triggers",
    "device_ids": ["device ids for OR event triggers"],
    "device_name": "device name for event triggers",
    "device_names": ["device names for OR event triggers"],
    "attribute": "e.g. isOpen",
    "value": true,
    "notification": "opened | closed"
  },
  "conditions": [
    {
      "type": "state | time_window | presence",
      "field": "optional field",
      "operator": "== | != | > | <",
      "value": "optional value"
    }
  ],
  "actions": [
    {
      "type": "set_device_attributes | trigger_scene | play_audio_clip",
      "target_id": "device-or-scene-id",
      "target_name": "human name",
      "attributes": {"isOn": true},
      "clip_name": "ikea://notify/03_device_open | ikea://notify/04_device_close",
      "volume": 20
    }
  ],
  "notes": ["review notes"],
  "apply_support": "unsupported | partial | supported"
}`
	system := "You are an IKEA DIRIGERA automation planner. Return JSON only. Generate a reviewable automation spec grounded in the supplied live home context. Do not invent device ids. For speaker sounds, only use built-in clip names ikea://notify/03_device_open or ikea://notify/04_device_close. Mark apply_support=supported only for automations representable as DIRIGERA scenes, especially event triggers and device/speaker actions."
	user := buildPrompt("Create an automation spec", prompt, home, schema)

	var spec autos.Spec
	if err := p.completeJSON(ctx, system, user, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func (p *OpenAICompatibleProvider) GenerateAskScript(ctx context.Context, prompt string, home HomeContext) (*AskScript, error) {
	schema := `{
  "mode": "answer | execute",
  "summary": "short summary",
  "requires_confirmation": false,
  "destructive": false,
  "notes": ["optional review notes"],
  "response": {
    "type": "table | list | text",
    "headers": ["COL1", "COL2"],
    "rows": [["value1", "value2"]],
    "items": ["value1", "value2"],
    "text": "plain text answer"
  },
  "actions": [
    {
      "type": "set_device_attributes | trigger_scene | rename_device | rename_scene | delete_device | delete_scene | set_speaker_volume | set_speaker_playback",
      "target_id": "device-or-scene-id",
      "target_name": "human name",
      "attributes": {"isOn": true, "lightLevel": 50},
      "value": "for speaker playback values like playbackPlaying or playbackPaused"
    }
  ],
  "script": {
    "language": "javascript",
    "runtime": "goja",
    "entry_point": "main",
    "timeout_seconds": 20,
    "capabilities": ["devices.setAttributes"],
    "source": "async function main(api) { await api.devices.setAttributes(\"device-id\", { isOn: false }); }"
  }
}`
	system := "You are an IKEA DIRIGERA script planner. Return JSON only. Generate a constrained JavaScript script for a sandboxed runtime. Users may reference devices by any value shown in the devices list columns NAME, ROOM, TYPE, or ID, including combinations like room plus type or room plus name. The supplied home context is inventory-first: it is authoritative for what devices and scenes exist, but not for always-current live attributes. The script must use only this host API: api.log(...args), api.logJson(value), api.devices.all(), api.devices.listByRoom(roomName), api.devices.listByType(type), api.devices.listByName(name), api.devices.getLiveByID(id), api.devices.getLiveByName(name), api.devices.listLiveByRoom(roomName), api.devices.listLiveByType(type), api.devices.listLiveByName(name), api.devices.setAttributes(id, attrs), api.devices.setAttributesByName(name, attrs), api.devices.setRoomAttributes(roomName, attrs), api.devices.isOpen(device), api.devices.isClosed(device), api.scenes.all(), api.scenes.trigger(id), api.scenes.triggerByName(name), api.speakers.all(), api.speakers.playSound(id, sound), api.speakers.playSoundByName(name, sound), api.speakers.playTest(id), api.speakers.playTestByName(name), api.state.get(key), api.state.set(key, value), api.state.delete(key), api.time.now(), api.guard.debounce(key, seconds), api.guard.throttle(key, seconds). Speaker sound names are sound-0 through sound-5, where playTest/playTestByName are aliases for sound-0. Prefer inventory helpers to discover targets, then use live helpers when the user needs current status or conditions. Live helper calls are asynchronous and must always be awaited. Live device state is exposed under attributes, for example: const live = await api.devices.getLiveByID(id); if (live && live.attributes && live.attributes.isOn === true) { ... }. Do not read live.isOn directly. For door/window/open-close sensors, prefer api.devices.isOpen(live) and api.devices.isClosed(live) instead of guessing raw attribute names; current devices may report fields such as attributes.isOpen or attributes.windowOpen. For room-wide or type-wide status queries, prefer api.devices.listLiveByRoom(...) or api.devices.listLiveByType(...), and those calls must also be awaited. For single-device conditions such as \"if PH Lampe is turned on\", prefer api.devices.getLiveByID(id) when the matching device id is known from the supplied home context; only use getLiveByName(name) when an exact id cannot be grounded. Likewise, when setting a resolved target, prefer api.devices.setAttributes(id, attrs) over name-based setters if the exact id is known. Use api.speakers.playSoundByName(name, 'sound-#') when the user asks for a numbered speaker sound on a named speaker, and use playTestByName only for generic test sound requests. Use api.guard.debounce/throttle instead of handwritten timing state when suppressing repeat runs. The script must use the supplied home context and should not invent device ids or scene ids. Also include a grounded action list that matches the script so the CLI can execute through a transitional runtime bridge. Do not generate fallback actions that target synthetic speakers like local-speaker through hub device patching; for speaker playback rely on the script runtime speaker API instead. For informational questions, return mode=answer with no actions. In answer mode, main(api) must return the actual answer object directly so the JavaScript alone is sufficient to reproduce the result on later runs. When the user asks for structured presentation such as a table, list, rows, columns, CSV-like output, or JSON-like output, do not return only summary text. Make main(api) return the structured result object. For tables return {type:\"table\", headers:[\"COL\"], rows:[[\"value\"]]}. For lists return {type:\"list\", items:[\"value\"]}. For plain informational prose return {type:\"text\", text:\"...\"}. The top-level response field, if provided, should mirror the same result returned by main(api). Structured responses must be grounded in the supplied home context and contain actual data rows/items, not header-only placeholders. If matching items exist in context, do not return empty rows/items. Keep NAME and ID separate when both matter. The summary should only summarize; it must not replace the returned result. For destructive actions set requires_confirmation=true and destructive=true."
	user := buildPrompt("Create an executable ask script", prompt, home, schema)
	key := promptCacheKey(p.baseURL, p.model, system, prompt, schema, home)
	if cached, ok, err := p.getCachedScript(key); err != nil {
		return nil, err
	} else if ok {
		p.debug("llm: ask codegen cache hit model=%s", p.model)
		return &AskScript{
			Mode:   "execute",
			Script: cached,
		}, nil
	}
	p.debug("llm: ask codegen cache miss model=%s", p.model)

	var plan AskScript
	if err := p.completeJSON(ctx, system, user, &plan); err != nil {
		return nil, err
	}
	if plan.Script.Language == "" {
		plan.Script.Language = script.LanguageJavaScript
	}
	if plan.Script.Runtime == "" {
		plan.Script.Runtime = script.RuntimeGoja
	}
	if plan.Script.EntryPoint == "" {
		plan.Script.EntryPoint = "main"
	}
	if reason := invalidAskScriptPlanReason(prompt, home, plan); reason != "" {
		repaired, err := p.repairAskScript(ctx, prompt, home, schema, system, plan, reason)
		if err != nil {
			return nil, err
		}
		plan = *repaired
	}
	if strings.TrimSpace(plan.Script.Source) != "" {
		if err := p.putCachedScript(key, plan.Script); err != nil {
			return nil, err
		}
	}
	return &plan, nil
}

func invalidAskScriptPlanReason(prompt string, home HomeContext, plan AskScript) string {
	source := plan.Script.Source
	source = strings.TrimSpace(source)
	if source != "" {
		usesLiveGetter := strings.Contains(source, "api.devices.getLiveByID(") ||
			strings.Contains(source, "api.devices.getLiveByName(") ||
			strings.Contains(source, "api.devices.listLiveByRoom(") ||
			strings.Contains(source, "api.devices.listLiveByType(") ||
			strings.Contains(source, "api.devices.listLiveByName(")
		if usesLiveGetter && !strings.Contains(source, "await api.devices.getLiveBy") && !strings.Contains(source, "await api.devices.listLiveBy") {
			return "live device helper calls must be awaited"
		}
		if strings.Contains(source, ".isOn") && !strings.Contains(source, "attributes.isOn") {
			return "live device state must be read from attributes.isOn"
		}
	}
	if strings.Contains(strings.ToLower(prompt), "test sound") {
		if speaker := resolveMentionedSpeaker(home, prompt); speaker != nil {
			speakerName := strings.TrimSpace(fmt.Sprint(speaker["name"]))
			if !strings.Contains(source, "api.speakers.playTest(") && !strings.Contains(source, "api.speakers.playTestByName(") && !strings.Contains(source, "api.speakers.playSound(") && !strings.Contains(source, "api.speakers.playSoundByName(") {
				return fmt.Sprintf("named speaker %q exists in inventory, so the script must use api.speakers.playTest/playTestByName or api.speakers.playSound/playSoundByName", speakerName)
			}
			texts := strings.ToLower(plan.Summary + "\n" + strings.Join(plan.Notes, "\n") + "\n" + fmt.Sprint(plan.Response))
			if strings.Contains(texts, "speaker") && strings.Contains(texts, "not found") {
				return fmt.Sprintf("named speaker %q exists in inventory, so the script must not claim the speaker was not found", speakerName)
			}
		}
	}
	return ""
}

func resolveMentionedSpeaker(home HomeContext, prompt string) map[string]any {
	normalizedPrompt := strings.ToLower(prompt)
	var partial map[string]any
	for _, device := range home.Devices {
		if !strings.EqualFold(fmt.Sprint(device["type"]), "speaker") {
			continue
		}
		name := strings.TrimSpace(fmt.Sprint(device["name"]))
		if name == "" {
			continue
		}
		lowerName := strings.ToLower(name)
		if strings.Contains(normalizedPrompt, lowerName) {
			return device
		}
		if partial == nil && strings.Contains(normalizedPrompt, strings.ToLower(strings.ReplaceAll(name, " ", ""))) {
			partial = device
		}
	}
	return partial
}

func (p *OpenAICompatibleProvider) repairAskScript(ctx context.Context, prompt string, home HomeContext, schema, system string, previous AskScript, reason string) (*AskScript, error) {
	previousJSON, _ := json.MarshalIndent(previous, "", "  ")
	repairPrompt := prompt + "\n\nThe previous script was invalid and must be regenerated.\nReason: " + reason + ".\nRules:\n- api.devices.getLiveByID(...), api.devices.getLiveByName(...), api.devices.listLiveByRoom(...), api.devices.listLiveByType(...), and api.devices.listLiveByName(...) are async and must be awaited.\n- Live device power state must be read from live.attributes.isOn.\n- Do not read live.isOn directly.\n- If the named speaker exists in the supplied inventory, do not claim it is missing; use api.speakers.playTestByName(name), api.speakers.playTest(id), api.speakers.playSoundByName(name, 'sound-#'), or api.speakers.playSound(id, 'sound-#').\n- Return a corrected script and corrected top-level response.\nPrevious invalid response:\n" + string(previousJSON)
	var repaired AskScript
	if err := p.completeJSON(ctx, system, buildPrompt("Create an executable ask script", repairPrompt, home, schema), &repaired); err != nil {
		return nil, err
	}
	if repaired.Script.Language == "" {
		repaired.Script.Language = script.LanguageJavaScript
	}
	if repaired.Script.Runtime == "" {
		repaired.Script.Runtime = script.RuntimeGoja
	}
	if repaired.Script.EntryPoint == "" {
		repaired.Script.EntryPoint = "main"
	}
	return &repaired, nil
}

func (p *OpenAICompatibleProvider) GenerateAutomationScript(ctx context.Context, prompt string, home HomeContext) (*AutomationScript, error) {
	schema := `{
  "summary": "short summary",
  "trigger": {
    "kind": "time | sun_event | manual | event",
    "at": "HH:MM",
    "sun_event": "sunrise | sunset",
    "offset": "+00:15",
    "cron": "optional cron",
    "time_zone": "IANA tz",
    "device_id": "device id for event triggers",
    "device_name": "device name for event triggers",
    "attribute": "e.g. isOpen",
    "value": true,
    "notification": "opened | closed"
  },
  "conditions": [
    {
      "type": "state | time_window | presence",
      "field": "optional field",
      "operator": "== | != | > | <",
      "value": "optional value"
    }
  ],
  "script": {
    "language": "javascript",
    "runtime": "goja",
    "entry_point": "main",
    "timeout_seconds": 30,
    "capabilities": ["devices.setAttributes", "scenes.trigger", "log"],
    "source": "async function main(api) { await api.devices.setAttributes(\"device-id\", { isOn: true }); }"
  },
  "notes": ["review notes"],
  "apply_support": "unsupported | partial | supported"
}`
	system := "You are an IKEA DIRIGERA automation script planner. Return JSON only. Generate a constrained JavaScript automation script grounded in the supplied inventory-first home context. Users may reference devices by any value shown in the devices list columns NAME, ROOM, TYPE, or ID, including combinations like room plus type or room plus name. Do not invent device ids. The script will run in a sandboxed runtime with only this API: api.log(...args), api.logJson(value), api.devices.all(), api.devices.listByRoom(roomName), api.devices.listByType(type), api.devices.listByName(name), api.devices.getLiveByID(id), api.devices.getLiveByName(name), api.devices.listLiveByRoom(roomName), api.devices.listLiveByType(type), api.devices.listLiveByName(name), api.devices.setAttributes(id, attrs), api.devices.setAttributesByName(name, attrs), api.devices.setRoomAttributes(roomName, attrs), api.devices.isOpen(device), api.devices.isClosed(device), api.scenes.all(), api.scenes.trigger(id), api.scenes.triggerByName(name), api.speakers.all(), api.speakers.playSound(id, sound), api.speakers.playSoundByName(name, sound), api.speakers.playTest(id), api.speakers.playTestByName(name), api.state.get(key), api.state.set(key, value), api.state.delete(key), api.time.now(), api.guard.debounce(key, seconds), api.guard.throttle(key, seconds). Speaker sound names are sound-0 through sound-5, where playTest/playTestByName are aliases for sound-0. Prefer inventory helpers to identify targets, then use live helpers when the automation depends on current status. For single-device conditions prefer api.devices.getLiveByID(id) when an exact id is grounded from the supplied context, otherwise use api.devices.getLiveByName(name). For door/window/open-close sensors, prefer api.devices.isOpen(live) and api.devices.isClosed(live) instead of guessing raw attribute names; current devices may report fields such as attributes.isOpen or attributes.windowOpen. For room-wide or type-wide status conditions prefer api.devices.listLiveByRoom(...) or api.devices.listLiveByType(...). When the user asks to play a numbered speaker sound, use api.speakers.playSoundByName(name, 'sound-#') or api.speakers.playSound(id, 'sound-#'). For generic test sound requests, use api.speakers.playTestByName(name) or api.speakers.playTest(id). Do not fake speaker playback with api.devices.setAttributes* and do not invent playback values like test_sound. Use api.guard.debounce/throttle instead of handwritten timing state when suppressing repeat runs. Do not use timers, networking, files, subprocesses, imports, require, fetch, or any API not listed here. The runner handles triggering separately, so the script should only implement the action body that runs when the trigger fires. Prefer event, time, or sun_event triggers when the user asks for automations. Use trigger.at for daily clock times, trigger.sun_event with optional trigger.offset for sunrise/sunset, and trigger.kind=event with exact trigger.device_id values grounded from the supplied context for device-driven automations. When the user refers to multiple devices with OR semantics, return trigger.device_ids and/or trigger.device_names with every grounded event source that should match. If the user says 'turned on or off' or 'state changes' for a device attribute, set trigger.attribute but leave trigger.value unset so any change of that attribute will match. Mark apply_support=supported when the automation uses event/time/sun_event and the script uses only the allowed runtime API."
	user := buildPrompt("Create an automation script", prompt, home, schema)

	var spec AutomationScript
	if err := p.completeJSON(ctx, system, user, &spec); err != nil {
		return nil, err
	}
	if spec.Script.Language == "" {
		spec.Script.Language = script.LanguageJavaScript
	}
	if spec.Script.Runtime == "" {
		spec.Script.Runtime = script.RuntimeGoja
	}
	if spec.Script.EntryPoint == "" {
		spec.Script.EntryPoint = "main"
	}
	canonicalizeAutomationScript(prompt, home, &spec)
	if reason := invalidAutomationScriptReason(prompt, home, spec); reason != "" {
		repaired, err := p.repairAutomationScript(ctx, prompt, home, schema, system, spec, reason)
		if err != nil {
			return nil, err
		}
		spec = *repaired
		canonicalizeAutomationScript(prompt, home, &spec)
	}
	return &spec, nil
}

func canonicalizeAutomationScript(prompt string, home HomeContext, spec *AutomationScript) {
	if spec == nil {
		return
	}
	if strings.EqualFold(spec.Trigger.Kind, "event") {
		spec.Trigger.DeviceIDs = canonicalizeDeviceRefs(home, spec.Trigger.DeviceIDs)
		spec.Trigger.DeviceNames = canonicalizeDeviceNames(home, spec.Trigger.DeviceNames)
		if device := resolveHomeDevice(home, spec.Trigger.DeviceID, spec.Trigger.DeviceName); device != nil {
			spec.Trigger.DeviceID = strings.TrimSpace(fmt.Sprint(device["id"]))
			name := strings.TrimSpace(fmt.Sprint(device["name"]))
			if name == "" {
				name = strings.TrimSpace(fmt.Sprint(device["custom_name"]))
			}
			spec.Trigger.DeviceName = name
		}
		if ids := extractMentionedDeviceIDs(prompt, home); len(ids) > 1 {
			spec.Trigger.DeviceIDs = ids
		}
		if mentionsOnOrOff(prompt) && strings.EqualFold(spec.Trigger.Attribute, "isOn") {
			spec.Trigger.Value = nil
		}
		if strings.Contains(strings.ToLower(prompt), "state changes") && strings.TrimSpace(spec.Trigger.Attribute) == "" {
			if inferred := inferEventAttribute(home, spec); inferred != "" {
				spec.Trigger.Attribute = inferred
			}
			spec.Trigger.Value = nil
		}
	}
}

func inferEventAttribute(home HomeContext, spec *AutomationScript) string {
	for _, device := range automationTriggerDevices(home, spec) {
		name := strings.ToLower(strings.TrimSpace(fmt.Sprint(device["name"])))
		deviceType := strings.ToLower(strings.TrimSpace(fmt.Sprint(device["type"])))
		if strings.Contains(name, "door") || strings.Contains(name, "window") {
			return "isOpen"
		}
		if deviceType == "sensor" {
			return "isOpen"
		}
	}
	return ""
}

func automationTriggerDevices(home HomeContext, spec *AutomationScript) []map[string]any {
	if spec == nil {
		return nil
	}
	out := make([]map[string]any, 0)
	seen := map[string]struct{}{}
	add := func(device map[string]any) {
		if device == nil {
			return
		}
		id := strings.TrimSpace(fmt.Sprint(device["id"]))
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, device)
	}
	for _, id := range spec.Trigger.DeviceIDs {
		add(resolveHomeDevice(home, id, ""))
	}
	for _, name := range spec.Trigger.DeviceNames {
		add(resolveHomeDevice(home, "", name))
	}
	add(resolveHomeDevice(home, spec.Trigger.DeviceID, spec.Trigger.DeviceName))
	return out
}

func canonicalizeDeviceRefs(home HomeContext, refs []string) []string {
	out := make([]string, 0, len(refs))
	seen := map[string]struct{}{}
	for _, ref := range refs {
		if device := resolveHomeDevice(home, ref, ""); device != nil {
			id := strings.TrimSpace(fmt.Sprint(device["id"]))
			if id != "" {
				if _, ok := seen[id]; !ok {
					seen[id] = struct{}{}
					out = append(out, id)
				}
			}
		}
	}
	return out
}

func canonicalizeDeviceNames(home HomeContext, refs []string) []string {
	out := make([]string, 0, len(refs))
	seen := map[string]struct{}{}
	for _, ref := range refs {
		if device := resolveHomeDevice(home, "", ref); device != nil {
			name := strings.TrimSpace(fmt.Sprint(device["name"]))
			if name == "" {
				name = strings.TrimSpace(fmt.Sprint(device["custom_name"]))
			}
			if name != "" {
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					out = append(out, name)
				}
			}
		}
	}
	return out
}

func extractMentionedDeviceIDs(prompt string, home HomeContext) []string {
	normalizedPrompt := strings.ToLower(prompt)
	out := make([]string, 0)
	seen := map[string]struct{}{}
	for _, device := range home.Devices {
		id := strings.TrimSpace(fmt.Sprint(device["id"]))
		if id == "" {
			continue
		}
		if strings.Contains(normalizedPrompt, strings.ToLower(id)) {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	return out
}

func resolveHomeDevice(home HomeContext, deviceID, deviceName string) map[string]any {
	trimmedID := strings.TrimSpace(deviceID)
	trimmedName := strings.TrimSpace(strings.ToLower(deviceName))
	for _, device := range home.Devices {
		if trimmedID != "" && strings.TrimSpace(fmt.Sprint(device["id"])) == trimmedID {
			return device
		}
		if trimmedName == "" {
			continue
		}
		for _, candidate := range []string{
			strings.TrimSpace(fmt.Sprint(device["name"])),
			strings.TrimSpace(fmt.Sprint(device["custom_name"])),
			strings.TrimSpace(fmt.Sprint(device["id"])),
		} {
			if strings.EqualFold(candidate, trimmedName) {
				return device
			}
		}
	}
	return nil
}

func mentionsOnOrOff(prompt string) bool {
	normalized := strings.ToLower(prompt)
	return strings.Contains(normalized, " on or off") ||
		strings.Contains(normalized, " off or on") ||
		strings.Contains(normalized, "turned on or off") ||
		strings.Contains(normalized, "turned off or on")
}

func invalidAutomationScriptReason(prompt string, home HomeContext, spec AutomationScript) string {
	deviceIDs := map[string]struct{}{}
	for _, device := range home.Devices {
		deviceIDs[strings.TrimSpace(fmt.Sprint(device["id"]))] = struct{}{}
	}
	if strings.TrimSpace(spec.Trigger.DeviceID) != "" {
		if _, ok := deviceIDs[strings.TrimSpace(spec.Trigger.DeviceID)]; !ok {
			return "trigger.device_id is not present in the supplied inventory"
		}
	}
	for _, id := range spec.Trigger.DeviceIDs {
		if _, ok := deviceIDs[strings.TrimSpace(id)]; !ok {
			return "trigger.device_ids contains a device id not present in the supplied inventory"
		}
	}
	source := strings.TrimSpace(spec.Script.Source)
	if (strings.Contains(strings.ToLower(prompt), "test sound") || strings.Contains(strings.ToLower(prompt), "sound-")) && !strings.Contains(source, "api.speakers.playTest") && !strings.Contains(source, "api.speakers.playSound") {
		return "speaker sound automations must use api.speakers.playTest/playTestByName or api.speakers.playSound/playSoundByName"
	}
	if strings.Contains(source, "test_sound") || strings.Contains(source, "playback") && strings.Contains(source, "setAttributesByName") {
		return "speaker test sound must not be implemented as a fake playback attribute patch"
	}
	return ""
}

func (p *OpenAICompatibleProvider) repairAutomationScript(ctx context.Context, prompt string, home HomeContext, schema, system string, previous AutomationScript, reason string) (*AutomationScript, error) {
	previousJSON, _ := json.MarshalIndent(previous, "", "  ")
	repairPrompt := prompt + "\n\nThe previous automation script was invalid and must be regenerated.\nReason: " + reason + ".\nRules:\n- trigger.device_id and every entry in trigger.device_ids must exactly match a device id from the supplied inventory.\n- If the user refers to multiple devices with OR semantics, return trigger.device_ids and/or trigger.device_names for all of them.\n- If the user says a device is turned on or off or that state changes, set trigger.attribute but leave trigger.value unset so any change matches.\n- Speaker sounds must use api.speakers.playTestByName(name), api.speakers.playTest(id), api.speakers.playSoundByName(name, 'sound-#'), or api.speakers.playSound(id, 'sound-#').\n- Do not fake speaker sound with api.devices.setAttributes* or playback=test_sound.\nPrevious invalid response:\n" + string(previousJSON)
	var repaired AutomationScript
	if err := p.completeJSON(ctx, system, buildPrompt("Create an automation script", repairPrompt, home, schema), &repaired); err != nil {
		return nil, err
	}
	if repaired.Script.Language == "" {
		repaired.Script.Language = script.LanguageJavaScript
	}
	if repaired.Script.Runtime == "" {
		repaired.Script.Runtime = script.RuntimeGoja
	}
	if repaired.Script.EntryPoint == "" {
		repaired.Script.EntryPoint = "main"
	}
	return &repaired, nil
}

func (p *OpenAICompatibleProvider) completeJSON(ctx context.Context, system, user string, into any) error {
	body := map[string]any{
		"model": p.model,
		"response_format": map[string]any{
			"type": "json_object",
		},
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	p.debug("llm: calling %s/chat/completions model=%s", p.baseURL, p.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		p.debug("llm: request failed err=%v", err)
		return err
	}
	defer resp.Body.Close()
	p.debug("llm: response status=%s", resp.Status)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("llm request failed with %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if len(envelope.Choices) == 0 || envelope.Choices[0].Message.Content == "" {
		return fmt.Errorf("llm response was empty")
	}
	content := strings.TrimSpace(envelope.Choices[0].Message.Content)
	if err := json.Unmarshal([]byte(content), into); err != nil {
		return fmt.Errorf("decode llm json: %w; raw: %s", err, content)
	}
	return nil
}

func (p *OpenAICompatibleProvider) getCachedScript(key string) (script.Document, bool, error) {
	var cached cachedScript
	if ok, err := p.cache.get(key, &cached); err != nil {
		return script.Document{}, false, err
	} else if ok && strings.TrimSpace(cached.Script.Source) != "" {
		return cached.Script, true, nil
	}
	return script.Document{}, false, nil
}

func (p *OpenAICompatibleProvider) putCachedScript(key string, doc script.Document) error {
	raw, err := json.Marshal(cachedScript{Script: doc})
	if err != nil {
		return err
	}
	return p.cache.put(key, raw)
}

func (p *OpenAICompatibleProvider) debug(format string, args ...any) {
	if p == nil || p.debugf == nil {
		return
	}
	p.debugf(format, args...)
}

func buildPrompt(prefix, prompt string, home HomeContext, schema string) string {
	homeJSON, _ := json.MarshalIndent(home, "", "  ")
	return fmt.Sprintf("%s.\nUser prompt:\n%s\n\nHome context:\n%s\n\nReturn JSON matching:\n%s", prefix, prompt, string(homeJSON), schema)
}
