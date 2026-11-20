package gamechat

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

type lkCalls struct {
	mu    sync.Mutex
	calls []string
}

func (l *lkCalls) add(method string, _ map[string]any) {
	l.mu.Lock()
	l.calls = append(l.calls, method)
	l.mu.Unlock()
}

func voiceRig(t *testing.T) (*rig, *lkCalls) {
	lk := &lkCalls{}
	return newRig(t, Deps{
		LiveKitURL: "wss://lk.example", LiveKitKey: "key", LiveKitSecret: "lksecret", LiveKitCall: lk.add,
	}), lk
}

func voiceMembers(d map[string]any) []map[string]any {
	raw, _ := d["members"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestVoiceOffWithoutLiveKit(t *testing.T) {
	r := newRig(t, Deps{})
	a, joined := r.join("T1", 1, RoleHost, "ali")
	v, _ := joined["voice"].(map[string]any)
	if v == nil || v["available"] != false || v["active"] != false {
		t.Fatalf("voice state: %v", joined["voice"])
	}
	a.send("voice_start", map[string]any{})
	if e := a.want("error"); e["code"] != "voice_unavailable" {
		t.Fatalf("code: %v", e["code"])
	}
}

func TestVoiceStartJoinLeave(t *testing.T) {
	r, lk := voiceRig(t)
	host, _ := r.join("T1", 1, RoleHost, "ali")
	watcher, _ := r.join("T1", 2, RoleWatcher, "veli")

	// A spectator can't open the voice chat, and can't join one that is off.
	watcher.send("voice_start", map[string]any{})
	if e := watcher.want("error"); e["code"] != "voice_players_only" {
		t.Fatalf("code: %v", e["code"])
	}
	watcher.send("voice_join", map[string]any{})
	if e := watcher.want("error"); e["code"] != "voice_off" {
		t.Fatalf("code: %v", e["code"])
	}

	host.send("voice_start", map[string]any{"muted": false})
	tok := host.want("voice_token")
	if tok["url"] != "wss://lk.example" || tok["room"] == "" {
		t.Fatalf("token frame: %v", tok)
	}
	parts := strings.Split(tok["token"].(string), ".")
	if len(parts) != 3 {
		t.Fatalf("token: %v", tok["token"])
	}
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(body, &claims)
	grant, _ := claims["video"].(map[string]any)
	if claims["iss"] != "key" || grant["room"] != tok["room"] || grant["roomJoin"] != true || grant["roomAdmin"] != nil {
		t.Fatalf("claims: %s", body)
	}

	// The spectator only SEES it — nobody is joined for them.
	st := watcher.want("voice")
	if st["active"] != true || st["by"] != "ali" || len(voiceMembers(st)) != 1 {
		t.Fatalf("state: %v", st["_raw"])
	}
	if ev, _ := st["event"].(map[string]any); ev == nil || ev["kind"] != "started" {
		t.Fatalf("event: %v", st["event"])
	}
	host.drain()

	watcher.send("voice_join", map[string]any{"muted": true})
	watcher.want("voice_token")
	st = host.want("voice")
	ms := voiceMembers(st)
	if len(ms) != 2 || ms[1]["name"] != "veli" || ms[1]["muted"] != true {
		t.Fatalf("members: %v", st["_raw"])
	}
	// The identity on the wire is the table key, never the account id.
	if ms[1]["sid"] == "2" || ms[1]["sid"] == "" {
		t.Fatalf("sid: %v", ms[1]["sid"])
	}

	watcher.send("voice_mute", map[string]any{"muted": false})
	if ms = voiceMembers(host.want("voice")); ms[1]["muted"] != false {
		t.Fatalf("mute not applied")
	}

	// The last one out ends the session.
	watcher.send("voice_leave", map[string]any{})
	host.want("voice")
	host.send("voice_leave", map[string]any{})
	st = watcher.want("voice")
	for st["active"] == true {
		st = watcher.want("voice")
	}
	lk.mu.Lock()
	defer lk.mu.Unlock()
	if len(lk.calls) == 0 || lk.calls[len(lk.calls)-1] != "DeleteRoom" {
		t.Fatalf("livekit calls: %v", lk.calls)
	}
}

func TestVoiceHostKickAndEnd(t *testing.T) {
	r, _ := voiceRig(t)
	host, _ := r.join("T1", 1, RoleHost, "ali")
	p2, j2 := r.join("T1", 2, RolePlayer, "veli")
	sid2, _ := j2["sid"].(string)

	host.send("voice_start", map[string]any{})
	host.want("voice_token")
	p2.send("voice_join", map[string]any{})
	p2.want("voice_token")
	host.drain()
	p2.drain()

	// A player can't moderate.
	p2.send("voice_end", map[string]any{})
	host.quiet("voice")

	host.send("voice_mute_user", map[string]any{"sid": sid2})
	st := p2.want("voice")
	if ev, _ := st["event"].(map[string]any); ev == nil || ev["kind"] != "muted" || ev["sid"] != sid2 {
		t.Fatalf("event: %v", st["_raw"])
	}

	host.send("voice_kick", map[string]any{"sid": sid2})
	st = p2.want("voice")
	if len(voiceMembers(st)) != 1 {
		t.Fatalf("still in: %v", st["_raw"])
	}
	p2.send("voice_join", map[string]any{})
	if e := p2.want("error"); e["code"] != "voice_kicked" {
		t.Fatalf("code: %v", e["code"])
	}

	host.send("voice_end", map[string]any{})
	st = p2.want("voice")
	if st["active"] != false {
		t.Fatalf("not ended: %v", st["_raw"])
	}
}
