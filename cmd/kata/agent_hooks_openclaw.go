package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/jsonutil"
)

const openClawManagedPrefix = "// kata-openclaw-managed: "

type openClawManaged struct {
	Version         int    `json:"version"`
	ID              string `json:"id"`
	Workspace       string `json:"workspace"`
	Executable      string `json:"executable"`
	Source          string `json:"source"`
	SourceSet       bool   `json:"sourceSet"`
	Contract        bool   `json:"contract"`
	Attention       bool   `json:"attention"`
	AddedEnabled    bool   `json:"addedEnabled"`
	AddedPermission bool   `json:"addedPermission"`
	AddedAllow      bool   `json:"addedAllow"`
	AddedPath       bool   `json:"addedPath"`
}

// planOpenClawAgentHooks only snapshots and reconciles this package and its
// explicit discovery/policy entry. JSON5/includes require native management:
// generic decoding would destroy comments or change include precedence.
func openClawAgentHookPaths(suppliedHome, suppliedConfig string) (string, string, error) {
	home := suppliedHome
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
	}
	if override := strings.TrimSpace(os.Getenv("OPENCLAW_HOME")); override != "" {
		home = openClawUserPath(override, home)
	}
	config := suppliedConfig
	if config == "" {
		config = strings.TrimSpace(os.Getenv("OPENCLAW_CONFIG_PATH"))
	}
	state := strings.TrimSpace(os.Getenv("OPENCLAW_STATE_DIR"))
	if state != "" {
		state = openClawUserPath(state, home)
	} else if config != "" {
		state = filepath.Dir(openClawUserPath(config, home))
	} else {
		suffix := ""
		if profile := strings.TrimSpace(os.Getenv("OPENCLAW_PROFILE")); profile != "" && !strings.EqualFold(profile, "default") {
			if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`).MatchString(profile) {
				return "", "", fmt.Errorf("OpenClaw profile name is invalid; select the same validated profile as the Gateway")
			}
			suffix = "-" + profile
		}
		state = filepath.Join(home, ".openclaw"+suffix)
	}
	if config == "" {
		config = filepath.Join(state, "openclaw.json")
	}
	var err error
	config, err = filepath.Abs(openClawUserPath(config, home))
	if err != nil {
		return "", "", err
	}
	return config, state, nil
}

func planOpenClawAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	var plan nativeAgentHookPlan
	if opts.Scope != "user" && opts.Scope != "project" {
		return plan, fmt.Errorf("OpenClaw scope must be user or project")
	}
	config, state, err := openClawAgentHookPaths(opts.Home, opts.ConfigPath)
	if err != nil {
		return plan, err
	}
	root := filepath.Join(state, "extensions")
	id := "kata-hooks-user"
	workspace := ""
	if opts.Scope == "project" {
		workspace, err = filepath.Abs(opts.Dir)
		if err != nil {
			return plan, err
		}
		root = filepath.Join(workspace, ".openclaw", "extensions")
		digest := sha256.Sum256([]byte(workspace))
		id = "kata-hooks-project-" + hex.EncodeToString(digest[:8])
	}
	root, err = filepath.Abs(filepath.Join(root, id))
	if err != nil {
		return plan, err
	}
	plan.Path = config
	paths := []string{filepath.Join(root, "index.js"), filepath.Join(root, "package.json"), filepath.Join(root, "openclaw.plugin.json"), config}
	for _, path := range paths {
		data, exists, e := readNativeAgentHookFile(path)
		if e != nil {
			return plan, e
		}
		plan.Changes = append(plan.Changes, nativeAgentHookChange{Path: path, Original: data, OriginalExists: exists})
	}
	raw := plan.Changes[3].Original
	cfg := map[string]any{}
	if len(raw) > 0 {
		if cfg, err = openClawDecodeConfig(raw); err != nil {
			return plan, fmt.Errorf("OpenClaw config must be standard JSON for Kata management; preserve JSON5 with native 'openclaw plugins install --link <package> --force' and enable/reload commands: %w", err)
		}
	}
	if cfg == nil {
		return plan, fmt.Errorf("OpenClaw config must be an object")
	}
	if openClawHasInclude(cfg) {
		return plan, fmt.Errorf("OpenClaw $include config requires native plugin management; use 'openclaw plugins install --link <package> --force', enable and reload; Kata will not flatten includes")
	}
	if (!remove || opts.Contract || opts.Attention) && (os.Getenv("OPENCLAW_NIX_MODE") == "1" || os.Getenv("OPENCLAW_CONFIG_READONLY") == "1") {
		return plan, fmt.Errorf("OpenClaw Nix/read-only mode requires declarative native plugin management")
	}
	old := openClawManaged{}
	owned := plan.Changes[0].OriginalExists
	if owned {
		line, _, ok := bytes.Cut(plan.Changes[0].Original, []byte("\n"))
		if !ok || !bytes.HasPrefix(line, []byte(openClawManagedPrefix)) || json.Unmarshal(line[len(openClawManagedPrefix):], &old) != nil || old.Version != 1 || old.ID != id || old.Workspace != workspace {
			return plan, fmt.Errorf("OpenClaw package %q is authored or unsupported; preserve it and choose another native package", root)
		}
		generated := openClawAssets(old)
		for i := range 3 {
			if !plan.Changes[i].OriginalExists || !bytes.Equal(plan.Changes[i].Original, generated[i]) {
				return plan, fmt.Errorf("OpenClaw owned package %q has authored edits; preserve it and reconcile with native plugin management", paths[i])
			}
		}
	} else {
		for i := 1; i < 3; i++ {
			if plan.Changes[i].OriginalExists {
				return plan, fmt.Errorf("OpenClaw package collision at %q; existing files are preserved", paths[i])
			}
		}
	}
	plugins, e := openClawObject(cfg, "plugins", !remove || owned)
	if e != nil {
		return plan, e
	}
	entries, e := openClawObject(plugins, "entries", !remove || owned)
	if e != nil {
		return plan, e
	}
	entry, e := openClawObject(entries, id, false)
	if e != nil {
		return plan, e
	}
	currentAuthorized := openClawAuthorized(plugins, entry, id)
	plan.CurrentContract = owned && old.Contract && currentAuthorized
	plan.CurrentAttentionStart = owned && old.Attention && currentAuthorized
	plan.CurrentAttentionEnd = plan.CurrentAttentionStart
	if remove && !opts.Contract && !opts.Attention {
		plan.Contract = plan.CurrentContract
		plan.AttentionStart = plan.CurrentAttentionStart
		plan.AttentionEnd = plan.CurrentAttentionEnd
		for i := range plan.Changes {
			plan.Changes[i].Content = plan.Changes[i].Original
			plan.Changes[i].Remove = !plan.Changes[i].OriginalExists
		}
		if owned && !currentAuthorized {
			plan.Warnings = append(plan.Warnings, "OpenClaw loading or prompt/workspace-capture permission is denied; inspect native enabled and hooks.allowConversationAccess/allowPromptInjection policy.")
		}
		plan.Warnings = append(plan.Warnings, "OpenClaw status is offline configuration evidence; native runtime loading is unverified.")
		return plan, nil
	}

	desired := old
	if !owned {
		desired = openClawManaged{Version: 1, ID: id, Workspace: workspace}
	}
	if remove {
		if opts.Contract {
			desired.Contract = false
		}
		if opts.Attention {
			desired.Attention = false
		}
	} else {
		if opts.Contract {
			desired.Contract = true
		}
		if opts.Attention {
			desired.Attention = true
		}
		if opts.Executable != "" {
			desired.Executable = opts.Executable
		}
		if opts.SourceSet {
			desired.Source = opts.Source
			desired.SourceSet = true
		}
		if desired.Executable == "" {
			return plan, fmt.Errorf("OpenClaw needs a resolved Kata executable")
		}
		if desired.SourceSet && desired.Source == "" {
			return plan, fmt.Errorf("--source requires a nonempty local file path")
		}
	}
	remaining := desired.Contract || desired.Attention
	if remaining {
		if entry == nil {
			if entries == nil {
				return plan, fmt.Errorf("OpenClaw plugin entries must be an object")
			}
			entry = map[string]any{}
			entries[id] = entry
		}
		if _, ok := entry["enabled"]; !ok {
			entry["enabled"] = true
			desired.AddedEnabled = true
		}
		hooks, e := openClawObject(entry, "hooks", true)
		if e != nil {
			return plan, e
		}
		if hooks == nil {
			return plan, fmt.Errorf("OpenClaw hooks must be an object")
		}
		if _, ok := hooks["allowConversationAccess"]; !ok {
			hooks["allowConversationAccess"] = true
			desired.AddedPermission = true
		}
		load, e := openClawObject(plugins, "load", true)
		if e != nil {
			return plan, e
		}
		changed, e := openClawAddString(load, "paths", root, true)
		if e != nil {
			return plan, e
		}
		desired.AddedPath = desired.AddedPath || changed
		if rawAllow, ok := plugins["allow"]; ok {
			allow, isList := rawAllow.([]any)
			if !isList || len(allow) > 0 {
				changed, e = openClawAddString(plugins, "allow", id, true)
				if e != nil {
					return plan, e
				}
				desired.AddedAllow = desired.AddedAllow || changed
			}
		}
		assets := openClawAssets(desired)
		for i := range 3 {
			plan.Changes[i].Content = assets[i]
		}
		authorized := openClawAuthorized(plugins, entry, id)
		plan.Contract = desired.Contract && authorized
		plan.AttentionStart = desired.Attention && authorized
		plan.AttentionEnd = plan.AttentionStart
		if !authorized {
			plan.Warnings = append(plan.Warnings, "OpenClaw loading or before_prompt_build permission is denied; contract and workspace-captured attention are unavailable. Review plugins.enabled, allow/deny, entries."+id+".enabled and hooks.allowConversationAccess/allowPromptInjection with native management; authored policy was preserved.")
		}
		plan.Warnings = append(plan.Warnings, "OpenClaw configuration is offline evidence only; reload the Gateway plugin and inspect native runtime registrations (API >=2026.9.7).")
	} else if owned {
		for i := range 3 {
			plan.Changes[i].Remove = true
		}
		if old.AddedEnabled && entry["enabled"] == true {
			delete(entry, "enabled")
		}
		hooks, e := openClawObject(entry, "hooks", false)
		if e != nil {
			return plan, e
		}
		if old.AddedPermission && hooks["allowConversationAccess"] == true {
			delete(hooks, "allowConversationAccess")
		}
		if len(hooks) == 0 {
			delete(entry, "hooks")
		}
		if len(entry) == 0 {
			delete(entries, id)
		}
		if old.AddedAllow {
			_, e = openClawAddString(plugins, "allow", id, false)
			if e != nil {
				return plan, e
			}
		}
		load, e := openClawObject(plugins, "load", false)
		if e != nil {
			return plan, e
		}
		if old.AddedPath {
			_, e = openClawAddString(load, "paths", root, false)
			if e != nil {
				return plan, e
			}
		}
	} else {
		for i := range 3 {
			plan.Changes[i].Remove = true
		}
	}
	encoded, err := json.Marshal(cfg, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return plan, err
	}
	encoded = append(encoded, '\n')
	// Preserve original bytes when the semantic config is unchanged.
	originalCfg, originalErr := openClawDecodeConfig(raw)
	if len(raw) > 0 && originalErr == nil {
		a, _ := json.Marshal(originalCfg, json.Deterministic(true))
		b, _ := json.Marshal(cfg, json.Deterministic(true))
		if bytes.Equal(a, b) {
			encoded = raw
		}
	}
	if !owned && remove {
		encoded = raw
		if !plan.Changes[3].OriginalExists {
			plan.Changes[3].Remove = true
		}
	}
	plan.Changes[3].Content = encoded
	return plan, nil
}

func openClawUserPath(value, home string) string {
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		return filepath.Join(home, value[2:])
	}
	return value
}

// Validate strict JSON (including duplicate members) before decoding numbers
// losslessly; unrelated provider config can contain integers beyond float64.
func openClawDecodeConfig(raw []byte) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(raw, &result, jsonutil.PreserveNumberLiterals()); err != nil {
		return nil, err
	}
	return result, nil
}
func openClawObject(parent map[string]any, key string, create bool) (map[string]any, error) {
	if parent == nil {
		if create {
			return nil, fmt.Errorf("OpenClaw %s parent must be an object", key)
		}
		return nil, nil
	}
	value, ok := parent[key]
	if !ok {
		if !create {
			return nil, nil
		}
		value = map[string]any{}
		parent[key] = value
	}
	result, ok := value.(map[string]any)
	if !ok || result == nil {
		return nil, fmt.Errorf("OpenClaw %s must be an object; preserve config and use native management", key)
	}
	return result, nil
}
func openClawAddString(parent map[string]any, key, value string, add bool) (bool, error) {
	if parent == nil {
		return false, nil
	}
	existing, exists := parent[key]
	if !exists && !add {
		return false, nil
	}
	values := []any{}
	if exists {
		var ok bool
		values, ok = existing.([]any)
		if !ok {
			return false, fmt.Errorf("OpenClaw %s must be a string array", key)
		}
	}
	found := -1
	for i, v := range values {
		text, ok := v.(string)
		if !ok {
			return false, fmt.Errorf("OpenClaw %s must be a string array", key)
		}
		if text == value {
			found = i
		}
	}
	if add {
		if found >= 0 {
			return false, nil
		}
		parent[key] = append(values, value)
		return true, nil
	}
	if found < 0 {
		return false, nil
	}
	parent[key] = append(values[:found], values[found+1:]...)
	return true, nil
}
func openClawAuthorized(plugins, entry map[string]any, id string) bool {
	if plugins == nil || entry == nil || plugins["enabled"] == false || entry["enabled"] != true {
		return false
	}
	if allow, ok := plugins["allow"].([]any); ok && len(allow) > 0 {
		found := false
		for _, v := range allow {
			found = found || v == id
		}
		if !found {
			return false
		}
	}
	if deny, ok := plugins["deny"].([]any); ok {
		for _, v := range deny {
			if v == id {
				return false
			}
		}
	}
	hooks, _ := entry["hooks"].(map[string]any)
	return hooks["allowConversationAccess"] == true && hooks["allowPromptInjection"] != false
}
func openClawHasInclude(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for k, item := range v {
			if k == "$include" || openClawHasInclude(item) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, openClawHasInclude)
	}
	return false
}
func openClawAssets(m openClawManaged) [3][]byte {
	metadata, _ := json.Marshal(m)
	code := openClawManagedPrefix + string(metadata) + "\n" + strings.ReplaceAll(openClawModule, "__OPTIONS__", string(metadata))
	pkg, _ := json.Marshal(map[string]any{"name": m.ID, "version": "1.0.0", "type": "module", "openclaw": map[string]any{"extensions": []string{"./index.js"}, "compat": map[string]string{"pluginApi": ">=2026.9.7", "minGatewayVersion": "2026.9.7"}}}, jsontext.WithIndent("  "), json.Deterministic(true))
	manifest, _ := json.Marshal(map[string]any{"id": m.ID, "name": "Kata agent hooks", "activation": map[string]bool{"onStartup": true}, "configSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}}, jsontext.WithIndent("  "), json.Deterministic(true))
	return [3][]byte{[]byte(code), append(pkg, '\n'), append(manifest, '\n')}
}

const openClawModule = `import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
const options = __OPTIONS__;
const run = promisify(execFile);
const stateSymbol = Symbol.for('kata.openclaw.hooks.v1');
const host = globalThis[stateSymbol] ||= { registrations: new Map(), sessions: new Map(), pending: new Map(), queues: new Map(), children: new Set(), childSessions: new Map(), launch: String(process.pid)+'-'+String(Math.round(Date.now()-process.uptime()*1000)) };
host.children ||= new Set();
host.childSessions ||= new Map();
const stateDir = path.join(os.tmpdir(),'kata-openclaw-'+String(process.getuid?.() ?? 'owner')+'-'+host.launch);
const statePath = path.join(stateDir,'sessions.json');
function privatePath(p, directory) {
 const s=fs.lstatSync(p);
 // Windows supplies synthetic POSIX modes; its temp directory uses native ACLs.
 const privateOwner=process.platform==='win32' || ((s.mode & 0o077)===0 && (process.getuid ? s.uid===process.getuid() : true));
 return !s.isSymbolicLink() && (directory?s.isDirectory():s.isFile()) && privateOwner;
}
function loadState() {
 try {
  if (!privatePath(stateDir,true) || !privatePath(statePath,false) || fs.statSync(statePath).size>262144) return;
  const rows=JSON.parse(fs.readFileSync(statePath,'utf8'));
  if (!Array.isArray(rows) || rows.length>64) return;
  for (const row of rows) if(row && typeof row.slot==='string' && typeof row.workspace==='string' && typeof row.ref==='string' && typeof row.session==='string' && typeof row.owner==='string' && typeof row.key==='string' && row.started===true && Date.now()-row.time<86400000) host.sessions.set(row.slot,row);
 } catch {}
}
function saveState() {
 try {
  fs.mkdirSync(stateDir,{mode:0o700});
 } catch {}
 try {
  if(!privatePath(stateDir,true))return;
  const rows=[...host.sessions.values()].filter(row=>Date.now()-row.time<86400000).slice(-64);
  const temp=path.join(stateDir,'sessions-'+options.id+'.tmp');
  const fd=fs.openSync(temp,fs.constants.O_WRONLY|fs.constants.O_CREAT|fs.constants.O_EXCL,0o600);
  try {fs.writeFileSync(fd,JSON.stringify(rows));}finally{fs.closeSync(fd)}
  fs.renameSync(temp,statePath);
 } catch {}
}
loadState();
function allowed(api,id=options.id) {
 const p=api.config?.plugins,e=p?.entries?.[id],h=e?.hooks;
 return p?.enabled!==false && e?.enabled===true && (!Array.isArray(p?.allow)||p.allow.length===0||p.allow.includes(id)) && !p?.deny?.includes(id) && h?.allowConversationAccess===true && h?.allowPromptInjection!==false;
}
function rememberChild(event,ctx) {
 const key=event?.childSessionKey||ctx?.childSessionKey;
 if(typeof key!=='string'||!key)return Promise.resolve();
 host.children.add(key);
 const tasks=[];
 for(const pendingKey of host.pending.keys()) {
  const parts=pendingKey.split('\u0000');
  if(parts.length===3&&parts[2]===key) {
   host.childSessions.set(parts[1],key);
   host.pending.delete(pendingKey);
  }
 }
 for(const row of host.sessions.values()) {
  if(row.key!==key)continue;
  host.childSessions.set(row.session,key);
  tasks.push(serial(row.slot,async()=>{
   if(host.sessions.get(row.slot)!==row)return;
   await reconcileChildAttention(row);
  }));
 }
 return Promise.all(tasks);
}
function childSession(session,key) {
 if(typeof key==='string'&&host.children.has(key))return key;
 const known=host.childSessions.get(session);
 return typeof known==='string'&&host.children.has(known)?known:undefined;
}
async function reconcileChildAttention(row) {
 if(host.sessions.get(row.slot)===row)host.sessions.delete(row.slot);
 const owner=[...host.sessions.values()].find(candidate=>candidate.workspace===row.workspace&&candidate.ref===row.ref&&candidate.started);
 try {
  if(owner)await serial(owner.slot,async()=>{
   if(host.sessions.get(owner.slot)!==owner||!owner.started) {
    await command(attentionArgs('end',row),row.workspace,500,row.executable||options.executable);
    return;
   }
   await command(attentionArgs('start',owner),owner.workspace,500,owner.executable||options.executable);
   if(host.sessions.get(owner.slot)!==owner||!owner.started)await command(attentionArgs('end',owner),owner.workspace,500,owner.executable||options.executable);
  });
  else await command(attentionArgs('end',row),row.workspace,500,row.executable||options.executable);
 }catch{if(owner&&host.sessions.get(owner.slot)===owner)owner.childFallback=row}
 saveState();
}
function serial(slot,task) {
 const previous=host.queues.get(slot)||Promise.resolve();
 const next=previous.catch(()=>{}).then(task);host.queues.set(slot,next);
 return next.finally(()=>{if(host.queues.get(slot)===next)host.queues.delete(slot)});
}
async function command(args,cwd,timeout=600,executable=options.executable) {
 return run(executable,args,{cwd,env:process.env,timeout,maxBuffer:16*1024*1024,windowsHide:true});
}
function attentionArgs(mode,row) {
 return ['agent-hooks','attention-native','openclaw',mode,'--session',row.session,'--host-pid',String(process.pid),'--ref',row.ref,'--workspace',row.workspace];
}
export default {
 id:options.id,
 register(api) {
  const registration={id:options.id,workspace:options.workspace,contract:options.contract,attention:options.attention,api,active:true};
  host.registrations.set(options.id,registration);
  function dispose(){registration.active=false;if(host.registrations.get(options.id)===registration)host.registrations.delete(options.id)}
  api.lifecycle?.onDispose?.(dispose);
  if(!api.lifecycle?.onDispose)api.registerService?.({id:options.id+'-ownership',start(){},stop:dispose});
  function owns(workspace,capability) {
   if(!registration.active || host.registrations.get(options.id)!==registration || !allowed(api) || (options.workspace && path.resolve(workspace)!==options.workspace))return false;
   if(options.workspace)return true;
   for(const r of host.registrations.values())if(r.active && r.workspace===path.resolve(workspace) && r[capability] && allowed(r.api,r.id))return false;
   return true;
  }
  api.on('before_prompt_build',async (_event,ctx)=>{
   const workspace=ctx.workspaceDir;
   if(typeof workspace!=='string'||!workspace)return;
   const contract=options.contract&&owns(workspace,'contract');
   const attention=options.attention&&owns(workspace,'attention');
   if(!contract&&!attention)return;
   try {
    ctx.hookInvocation?.assertActive();
    const session=ctx.sessionId,key=ctx.sessionKey||ctx.agentId;
    if(attention && typeof session==='string' && typeof key==='string') {
     const slot=path.resolve(workspace)+'\u0000'+key;
     await serial(slot,async()=>{
      ctx.hookInvocation?.assertActive();
      if(!owns(workspace,'attention'))return;
      if(childSession(session,key)) {
       host.pending.delete(options.id+'\u0000'+session+'\u0000'+key);
       return;
      }
      const existing=host.sessions.get(slot);
      if(existing?.session===session&&existing.started){
       existing.owner=options.id;
       if(existing.childFallback)try {
        await command(attentionArgs('start',existing),existing.workspace,500,existing.executable||options.executable);
        if(host.sessions.get(slot)===existing&&existing.started)delete existing.childFallback;
       }catch{}
       saveState();return;
      }
      const pendingKey=options.id+'\u0000'+session+'\u0000'+key;
      const ref=host.pending.get(pendingKey)?.ref || process.env.KATA_REF;
      host.pending.delete(pendingKey);
      if(!ref)return;
      const row={slot,session,key,workspace:path.resolve(workspace),ref,owner:options.id,executable:options.executable,started:true,time:Date.now()};
      try {
       await command(attentionArgs('start',row),row.workspace,500);
       if(childSession(session,key)){await reconcileChildAttention(row);return}
       host.sessions.set(slot,row);while(host.sessions.size>64)host.sessions.delete(host.sessions.keys().next().value);saveState();
      }catch{}
     });
     ctx.hookInvocation?.assertActive();
    }
    if(!contract)return;
    const source=['agent-contract-hook'];if(options.sourceSet)source.push('--source',options.source);
    const recipient=process.env.KATA_INBOX_USER;
    const [contractRead,inboxRead]=await Promise.allSettled([command(source,workspace),recipient?command(['inbox','--for',recipient,'--context'],workspace):Promise.resolve({stdout:''})]);
    ctx.hookInvocation?.assertActive();
    if(!owns(workspace,'contract'))return;
    let system;
    if(contractRead.status==='fulfilled')try {const text=JSON.parse(contractRead.value.stdout).hookSpecificOutput?.additionalContext;if(typeof text==='string')system=text}catch{}
    const inbox=inboxRead.status==='fulfilled'?inboxRead.value.stdout:'Kata inbox unavailable for this prompt.';
    return {...(system!==undefined?{prependSystemContext:system}:{}),prependContext:inbox};
   }catch{return}
  },{timeoutMs:1500});
  if(options.attention) {
   // These accepted-spawn hooks carry OpenClaw's exact child session key.
   // Keep contract injection available in child prompts; attention uses only
   // verified lifecycle identity, never session-key spelling heuristics.
   api.on('subagent_progress',(event,ctx)=>event.phase==='started'?rememberChild(event,ctx):undefined, {timeoutMs:700});
   api.on('subagent_spawned',rememberChild, {timeoutMs:700});
   // Native session_start has no verified workspace. The first prompt captures
   // it, including image-only and resumed sessions; start is idempotent per host.
   api.on('session_start',(event,ctx)=>{
    const session=event.sessionId||ctx.sessionId,key=event.sessionKey||ctx.sessionKey||ctx.agentId,ref=process.env.KATA_REF;
    if(registration.active && typeof session==='string' && typeof key==='string' && host.children.has(key)){
     host.childSessions.set(session,key);
     return;
    }
    if(registration.active && typeof session==='string' && typeof key==='string' && ref){
     host.pending.set(options.id+'\u0000'+session+'\u0000'+key,{ref});
     while(host.pending.size>64)host.pending.delete(host.pending.keys().next().value);
    }
   }, {timeoutMs:700});
   api.on('session_end',async(event,ctx)=>{
    const session=event.sessionId||ctx.sessionId,key=event.sessionKey||ctx.sessionKey;
    if(typeof session!=='string')return;
    const child=childSession(session,key);
    if(child){
     host.childSessions.delete(session);
     host.children.delete(child);
     host.pending.delete(options.id+'\u0000'+session+'\u0000'+child);
     return;
    }
    // A project can unload or lose authority before another prompt. Terminal
    // responsibility follows active scope policy and the captured executable.
    const rows=[...host.sessions.values()].filter(row=>row.session===session&&(!key||row.key===key));
    const deadline=Date.now()+750;
    const work=Promise.all(rows.map(row=>serial(row.slot,async()=>{
     const executable=row.executable || (row.owner===options.id?options.executable:undefined);
     if(Date.now()>=deadline || !executable || !owns(row.workspace,'attention')||host.sessions.get(row.slot)!==row||!row.started)return;
     if(row.childFallback) {
      let complete=true;
      for(const owner of [row,row.childFallback]) {
       if(Date.now()>=deadline){complete=false;break}
       try {await command(attentionArgs('end',owner),owner.workspace,Math.min(650,Math.max(1,deadline-Date.now())),owner.executable||executable)}catch{complete=false}
      }
      if(complete&&host.sessions.get(row.slot)===row){host.sessions.delete(row.slot);saveState()}
      return;
     }
     try {await command(attentionArgs('end',row),row.workspace,Math.min(650,Math.max(1,deadline-Date.now())),executable);if(host.sessions.get(row.slot)===row)host.sessions.delete(row.slot);saveState()}catch{}
    })));
    let timer;try {await Promise.race([work,new Promise(resolve=>{timer=setTimeout(resolve,750)})])}finally{clearTimeout(timer)}
   },{timeoutMs:900});
  }
 }
};
`
