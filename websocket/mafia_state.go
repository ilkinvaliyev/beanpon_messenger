package websocket

import (
	"encoding/json"
	"time"
)

// ============================================================================
// MAFIA OYUNU — Vəziyyət (State) strukturları
//
// Bütün oyun vəziyyəti live_rooms.active_game (jsonb) sahəsində saxlanır.
// Server hər mərhələdən sonra bura yazır; reconnect / sonradan girən üçün
// buradan oxunur. Oyun bitəndə active_game = NULL olur.
//
// TƏHLÜKƏSİZLİK QAYDASI: Role, MafiaTeam, NightActions sahələri HEÇ VAXT
// ümumi broadcast-da getmir. Yalnız serverdə + fərdi olaraq aid olduğu
// oyunçuya (c.Send <-) göndərilir. Ümumi görünüş buildPublicState ilə
// maskalanır.
// ============================================================================

// Mərhələ adları
const (
	MafiaPhaseIntro     = "intro"          // Kart göstərilir + "Hazırım" (12s)
	MafiaPhaseNight     = "night"          // Don vurur + Dedektiv seçir
	MafiaPhaseDay       = "day_discussion" // Səslər açıq, müzakirə (dinamik timer)
	MafiaPhaseVote      = "day_voting"     // İlk səsvermə (kim mahkəməyə düşür)
	MafiaPhaseDefense   = "defense"        // Mahkəmədəkilər sırayla müdafiə (1dəq)
	MafiaPhaseFinalVote = "final_voting"   // Son səsvermə
	MafiaPhaseExecution = "execution"      // Ən çoxlu çıxır, 30s reaksiya
	MafiaPhaseEnded     = "ended"          // Oyun bitdi
)

// Rol açarları (Laravel mafia_roles.key ilə uyğun)
const (
	RoleMafia     = "mafia"
	RoleDon       = "don"
	RoleDetective = "detective"
	RoleCitizen   = "citizen"
)

// Tərəflər
const (
	TeamMafia = "mafia"
	TeamTown  = "town"
)

// Qalib
const (
	WinnerMafia = "mafia"
	WinnerTown  = "town"
)

// MafiaPlayer — bir oyunçunun vəziyyəti.
type MafiaPlayer struct {
	UserID   uint    `json:"user_id"`
	Name     string  `json:"name"`
	Avatar   *string `json:"avatar"`
	Role     string  `json:"role"` // ⚠️ broadcast-da maskalanır
	Alive    bool    `json:"alive"`
	IsReady  bool    `json:"is_ready"`  // "Hazırım"a basıb-basmadığı
	HasVoted bool    `json:"has_voted"` // cari səsvermədə səs verib-vermədiyi
}

// MafiaNightActions — cari gecə yığılan gizli hərəkətlər (yalnız serverdə).
type MafiaNightActions struct {
	DonTarget       *uint `json:"don_target,omitempty"`       // Don kimi vurur
	DetectiveTarget *uint `json:"detective_target,omitempty"` // Dedektiv kimi seçir
}

// MafiaHistoryEntry — sabah elanları üçün hadisə qeydi.
type MafiaHistoryEntry struct {
	Day     int    `json:"day"`
	Event   string `json:"event"` // killed_by_mafia, found_by_detective, lynched, detective_died, tie
	UserID  *uint  `json:"user_id,omitempty"`
	Role    string `json:"role,omitempty"`
	Message string `json:"message"` // chat-a düşən mətn
}

// MafiaGame — tam oyun vəziyyəti (active_game jsonb).
type MafiaGame struct {
	Type        string         `json:"type"` // həmişə "mafia"
	Phase       string         `json:"phase"`
	DayNumber   int            `json:"day_number"`
	PhaseEndsAt time.Time      `json:"phase_ends_at"`
	Players     []*MafiaPlayer `json:"players"`

	// ⚠️ Aşağıdakılar broadcast-da getmir, yalnız serverdə + fərdi göndərilir
	MafiaTeam    []uint            `json:"mafia_team,omitempty"`    // mafia user_id-ləri
	NightActions MafiaNightActions `json:"night_actions,omitempty"` // cari gecə

	// Gündüz səsvermə: voterID -> targetID
	Votes map[uint]uint `json:"votes"`
	// Mahkəmədə olanlar (2+ səs alanlar)
	OnTrial []uint `json:"on_trial,omitempty"`
	// Müdafiə növbəsi (OnTrial-ın indeksi)
	DefenseIndex int `json:"defense_index"`

	History []MafiaHistoryEntry `json:"history"`
	Winner  string              `json:"winner,omitempty"` // mafia | town | ""
}

// --- (de)serializasiya: active_game jsonb <-> MafiaGame ---

// MarshalGame — MafiaGame-i jsonb üçün json.RawMessage-ə çevirir.
func MarshalGame(g *MafiaGame) (json.RawMessage, error) {
	b, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// UnmarshalGame — active_game jsonb-dən MafiaGame oxuyur.
// Mafia oyunu deyilsə (type != "mafia") nil qaytarır.
func UnmarshalGame(raw json.RawMessage) (*MafiaGame, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var g MafiaGame
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, false
	}
	if g.Type != "mafia" {
		return nil, false
	}
	return &g, true
}

// --- köməkçi axtarış funksiyaları ---

func (g *MafiaGame) findPlayer(userID uint) *MafiaPlayer {
	for _, p := range g.Players {
		if p.UserID == userID {
			return p
		}
	}
	return nil
}

func (g *MafiaGame) alivePlayers() []*MafiaPlayer {
	var out []*MafiaPlayer
	for _, p := range g.Players {
		if p.Alive {
			out = append(out, p)
		}
	}
	return out
}

func (g *MafiaGame) isMafiaRole(role string) bool {
	return role == RoleMafia || role == RoleDon
}

func (g *MafiaGame) aliveMafiaCount() int {
	c := 0
	for _, p := range g.Players {
		if p.Alive && g.isMafiaRole(p.Role) {
			c++
		}
	}
	return c
}

func (g *MafiaGame) aliveTownCount() int {
	c := 0
	for _, p := range g.Players {
		if p.Alive && !g.isMafiaRole(p.Role) {
			c++
		}
	}
	return c
}

// ============================================================================
// MASKALANMA: buildPublicState
//
// Hər istifadəçi üçün düzgün görünüşü qaytarır:
//   - Sağ oyunçu: öz rolu görünür; mafiasa komandası işarələnir.
//   - İzləyici/başqa oyunçu: yalnız ölənlərin rolu görünür, sağların rolu GİZLİ.
//   - Mafialar bir-birini "is_teammate=true" ilə görür.
// Bütün broadcast bu funksiyadan keçməlidir.
// ============================================================================

type PublicPlayer struct {
	UserID     uint    `json:"user_id"`
	Name       string  `json:"name"`
	Avatar     *string `json:"avatar"`
	Alive      bool    `json:"alive"`
	Role       string  `json:"role,omitempty"`        // yalnız ölü / oyun bitib / özü
	IsTeammate bool    `json:"is_teammate,omitempty"` // mafia bir-birini görür
	IsSelf     bool    `json:"is_self,omitempty"`
	IsOnTrial  bool    `json:"is_on_trial,omitempty"`
}

// buildPublicState — forUserID üçün maskalanmış vəziyyət qaytarır.
// forUserID == 0 → təmiz izləyici (heç bir gizli məlumat).
func (g *MafiaGame) buildPublicState(forUserID uint) map[string]interface{} {
	viewer := g.findPlayer(forUserID)
	viewerIsMafia := viewer != nil && g.isMafiaRole(viewer.Role)
	gameOver := g.Phase == MafiaPhaseEnded

	onTrialSet := make(map[uint]bool, len(g.OnTrial))
	for _, id := range g.OnTrial {
		onTrialSet[id] = true
	}

	publicPlayers := make([]PublicPlayer, 0, len(g.Players))
	for _, p := range g.Players {
		pp := PublicPlayer{
			UserID:    p.UserID,
			Name:      p.Name,
			Avatar:    p.Avatar,
			Alive:     p.Alive,
			IsOnTrial: onTrialSet[p.UserID],
		}

		isSelf := viewer != nil && p.UserID == forUserID
		pp.IsSelf = isSelf

		// Rol nə vaxt açıqlanır:
		//  - oyun bitib (hamı görür)
		//  - oyunçu ölüb (kartı açılıb)
		//  - özüdürsə (öz rolunu görür)
		//  - viewer mafiadır və bu oyunçu da mafiadır (komanda)
		switch {
		case gameOver || !p.Alive || isSelf:
			pp.Role = p.Role
		case viewerIsMafia && g.isMafiaRole(p.Role):
			pp.Role = p.Role // mafia komandasının rolunu görür (Don/mafia)
			pp.IsTeammate = true
		default:
			// gizli — sağ oyunçunun rolu izləyiciyə/qarşı tərəfə getmir
		}

		publicPlayers = append(publicPlayers, pp)
	}

	state := map[string]interface{}{
		"type":          "mafia",
		"phase":         g.Phase,
		"day_number":    g.DayNumber,
		"phase_ends_at": g.PhaseEndsAt,
		"players":       publicPlayers,
		"votes":         g.Votes,
		"on_trial":      g.OnTrial,
		"defense_index": g.DefenseIndex,
		"history":       g.History,
		"winner":        g.Winner,
	}

	// Viewer mafiadırsa komanda siyahısını da əlavə et (öz tərəfini bilsin)
	if viewerIsMafia {
		state["mafia_team"] = g.MafiaTeam
	}

	// Viewer-in öz rolu (UI kart üçün rahat olsun)
	if viewer != nil {
		state["my_role"] = viewer.Role
		state["my_alive"] = viewer.Alive
	}

	return state
}
