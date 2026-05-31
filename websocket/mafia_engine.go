package websocket

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// ============================================================================
// MAFIA OYUNU — Saf məntiq (engine)
//
// Bu fayldakı funksiyalar WebSocket-dən asılı deyil. Yalnız MafiaGame
// vəziyyətini oxuyub-dəyişir. live_hub / handler bu funksiyaları çağırıb
// nəticəni broadcast edir.
// ============================================================================

// Mərhələ müddətləri
const (
	mafiaIntroSeconds     = 12 // kart + Hazırım
	mafiaVoteSeconds      = 30 // ilk səsvermə
	mafiaDefenseSeconds   = 60 // hər müdafiə (1 dəq)
	mafiaFinalVoteSeconds = 30 // son səsvermə
	mafiaExecutionSeconds = 30 // çıxış reaksiyası
	mafiaNightSeconds     = 30 // gecə (Don + Dedektiv)
)

// Oyunçu sayı limitləri
const (
	// MafiaMinPlayers = 5 // ← normal limit (test bitəndə bunu aç, aşağıdakını sil)
	MafiaMinPlayers = 3 // TEST: müvəqqəti 3 nəfər
	MafiaMaxPlayers = 12
)

// dayDiscussionSeconds — sağ oyunçu sayına görə dinamik müzakirə timer-i.
//
//	< 7 nəfər  → 3 dəqiqə
//	8–10 nəfər → 4 dəqiqə
//	11–12      → 6 dəqiqə
func dayDiscussionSeconds(aliveCount int) int {
	switch {
	case aliveCount <= 7:
		return 3 * 60
	case aliveCount <= 10:
		return 4 * 60
	default:
		return 6 * 60
	}
}

// roleCount — bir rol dəstini təmsil edir.
type roleCount struct {
	mafia     int // Don xaric adi mafia sayı
	don       int // 0 və ya 1
	detective int
	citizen   int
}

// rolesByPlayerCount — oyunçu sayına görə sabit rol cədvəli (plan §1).
var rolesByPlayerCount = map[int]roleCount{
	// TEST: 3-4 nəfər (müvəqqəti). Normal limit 5 olanda bunları silə bilərsən.
	3:  {mafia: 1, don: 0, detective: 1, citizen: 1},
	4:  {mafia: 1, don: 0, detective: 1, citizen: 2},
	5:  {mafia: 1, don: 0, detective: 1, citizen: 3},
	6:  {mafia: 1, don: 0, detective: 1, citizen: 4},
	7:  {mafia: 1, don: 1, detective: 1, citizen: 4},
	8:  {mafia: 1, don: 1, detective: 1, citizen: 5},
	9:  {mafia: 2, don: 1, detective: 1, citizen: 6},
	10: {mafia: 2, don: 1, detective: 1, citizen: 7},
	11: {mafia: 2, don: 1, detective: 1, citizen: 8},
	12: {mafia: 3, don: 1, detective: 1, citizen: 9},
}

// assignRoles — verilmiş oyunçulara plan §1 cədvəlinə görə rolları gizli
// paylayır. Oyunçu sıralaması qarışdırılır ki, rollar random düşsün.
// Geri qaytarır: mafia komandasının user_id-ləri.
func assignRoles(players []*MafiaPlayer) ([]uint, error) {
	n := len(players)
	rc, ok := rolesByPlayerCount[n]
	if !ok {
		return nil, fmt.Errorf("dəstəklənməyən oyunçu sayı: %d (5-12 olmalıdır)", n)
	}

	// Random sıra
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Shuffle(len(players), func(i, j int) {
		players[i], players[j] = players[j], players[i]
	})

	idx := 0
	var mafiaTeam []uint

	// Don
	for i := 0; i < rc.don; i++ {
		players[idx].Role = RoleDon
		mafiaTeam = append(mafiaTeam, players[idx].UserID)
		idx++
	}
	// Adi mafia
	for i := 0; i < rc.mafia; i++ {
		players[idx].Role = RoleMafia
		mafiaTeam = append(mafiaTeam, players[idx].UserID)
		idx++
	}
	// Dedektiv
	for i := 0; i < rc.detective; i++ {
		players[idx].Role = RoleDetective
		idx++
	}
	// Sakin
	for i := 0; i < rc.citizen; i++ {
		players[idx].Role = RoleCitizen
		idx++
	}

	// Hamısı sağ, hazır deyil
	for _, p := range players {
		p.Alive = true
		p.IsReady = false
		p.HasVoted = false
	}

	return mafiaTeam, nil
}

// allReady — bütün sağ oyunçular "Hazırım"a basıbsa true.
func (g *MafiaGame) allReady() bool {
	for _, p := range g.Players {
		if p.Alive && !p.IsReady {
			return false
		}
	}
	return true
}

// allVoted — bütün sağ oyunçular səs veribsə true.
func (g *MafiaGame) allVoted() bool {
	for _, p := range g.Players {
		if p.Alive && !p.HasVoted {
			return false
		}
	}
	return true
}

// resetVotes — yeni səsvermə üçün səsləri təmizlə.
func (g *MafiaGame) resetVotes() {
	g.Votes = make(map[uint]uint)
	for _, p := range g.Players {
		p.HasVoted = false
	}
}

// ============================================================================
// GECƏ RESOLVE — plan §6
//
// Sıra:
//  1. Mafia (Don) vurdu → o oyunçu ölür.
//  2. Dedektiv seçdi:
//     - seçdiyi mafia/Don-dursa → o da çıxır, hamı bilir.
//     - sakindirsə → "boşluğa getdi" (hədəf gizli).
//  3. İSTİSNA: mafia dedektivi həmin gecə vurmuşdusa → dedektivin seçimi
//     keçərsiz (mafia birinci hərəkət etdi).
//
// Bir gecədə 2 ölüm mümkündür.
// ============================================================================
func (g *MafiaGame) resolveNight() {
	na := g.NightActions
	day := g.DayNumber

	// Dedektiv kim idi (sağ)?
	var detective *MafiaPlayer
	for _, p := range g.Players {
		if p.Alive && p.Role == RoleDetective {
			detective = p
			break
		}
	}

	detectiveKilledByMafia := false

	// 1) Mafia (Don) vurdu
	if na.DonTarget != nil {
		victim := g.findPlayer(*na.DonTarget)
		if victim != nil && victim.Alive {
			victim.Alive = false
			if detective != nil && victim.UserID == detective.UserID {
				detectiveKilledByMafia = true
			}
			vid := victim.UserID
			g.History = append(g.History, MafiaHistoryEntry{
				Day:     day,
				Event:   "killed_by_mafia",
				UserID:  &vid,
				Role:    victim.Role,
				Message: fmt.Sprintf("Gecə %s öldürüldü. (%s)", victim.Name, roleLabel(victim.Role)),
			})
		}
	}

	// 2) Dedektiv seçdi
	if na.DetectiveTarget != nil && detective != nil {
		if detectiveKilledByMafia {
			// 3) İSTİSNA — mafia dedektivi daha tez öldürdü
			g.History = append(g.History, MafiaHistoryEntry{
				Day:     day,
				Event:   "detective_died",
				Message: "Dedektiv mafianı tapmışdı, amma mafia onu daha tez öldürdü.",
			})
		} else {
			target := g.findPlayer(*na.DetectiveTarget)
			if target != nil && target.Alive && g.isMafiaRole(target.Role) {
				// Düz tapdı → mafia çıxır, hamı bilir
				target.Alive = false
				tid := target.UserID
				g.History = append(g.History, MafiaHistoryEntry{
					Day:     day,
					Event:   "found_by_detective",
					UserID:  &tid,
					Role:    target.Role,
					Message: fmt.Sprintf("Dedektiv mafianı tapdı — %s çıxarıldı. (%s)", target.Name, roleLabel(target.Role)),
				})
			} else {
				// Səhv etdi → boşluğa getdi, hədəf gizli
				g.History = append(g.History, MafiaHistoryEntry{
					Day:     day,
					Event:   "detective_miss",
					Message: "Dedektiv boşluğa getdi.",
				})
			}
		}
	}

	// Don ölübsə → random başqa mafiaya Don rolu keç
	g.reassignDonIfNeeded()

	// Gecə hərəkətlərini təmizlə
	g.NightActions = MafiaNightActions{}
}

// reassignDonIfNeeded — sağ Don yoxdursa, amma sağ adi mafia varsa,
// random birinə Don rolunu verir (plan §6).
func (g *MafiaGame) reassignDonIfNeeded() {
	hasAliveDon := false
	var aliveMafias []*MafiaPlayer
	for _, p := range g.Players {
		if !p.Alive {
			continue
		}
		if p.Role == RoleDon {
			hasAliveDon = true
		} else if p.Role == RoleMafia {
			aliveMafias = append(aliveMafias, p)
		}
	}
	if hasAliveDon || len(aliveMafias) == 0 {
		return
	}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	newDon := aliveMafias[r.Intn(len(aliveMafias))]
	newDon.Role = RoleDon
}

// ============================================================================
// SƏSVERMƏ RESOLVE
// ============================================================================

// tallyVotes — targetID -> aldığı səs sayı.
func (g *MafiaGame) tallyVotes() map[uint]int {
	counts := make(map[uint]int)
	for _, target := range g.Votes {
		counts[target]++
	}
	return counts
}

// resolveFirstVote — 2+ səs alanları mahkəməyə (OnTrial) qoyur.
// Səs sırasına görə sıralanır. Heç kim 2 səs almasa OnTrial boş qalır.
func (g *MafiaGame) resolveFirstVote() {
	counts := g.tallyVotes()
	var trial []uint
	for uid, c := range counts {
		if c >= 2 {
			trial = append(trial, uid)
		}
	}
	// Çox səsdən aza, bərabərdə user_id-yə görə (deterministik)
	sort.Slice(trial, func(i, j int) bool {
		if counts[trial[i]] != counts[trial[j]] {
			return counts[trial[i]] > counts[trial[j]]
		}
		return trial[i] < trial[j]
	})
	g.OnTrial = trial
	g.DefenseIndex = 0
}

// resolveFinalVote — son səsvermədə ən çox səs alanı çıxarır.
// Bərabərlik olarsa heç kim çıxmır (lynchedID = nil).
// Geri qaytarır: çıxarılan oyunçu (varsa).
func (g *MafiaGame) resolveFinalVote() *MafiaPlayer {
	counts := g.tallyVotes()
	if len(counts) == 0 {
		g.History = append(g.History, MafiaHistoryEntry{
			Day:     g.DayNumber,
			Event:   "tie",
			Message: "Səslər bərabərə, kimsə çıxmadı. Gecə başlayır.",
		})
		return nil
	}

	// Maksimumu tap
	maxVotes := 0
	for _, c := range counts {
		if c > maxVotes {
			maxVotes = c
		}
	}
	// Maksimuma sahib olanlar
	var top []uint
	for uid, c := range counts {
		if c == maxVotes {
			top = append(top, uid)
		}
	}

	// Bərabərlik → kimsə çıxmır
	if len(top) != 1 {
		g.History = append(g.History, MafiaHistoryEntry{
			Day:     g.DayNumber,
			Event:   "tie",
			Message: "Səslər bərabərə, kimsə çıxmadı. Gecə başlayır.",
		})
		return nil
	}

	lynched := g.findPlayer(top[0])
	if lynched == nil || !lynched.Alive {
		return nil
	}
	lynched.Alive = false
	lid := lynched.UserID
	g.History = append(g.History, MafiaHistoryEntry{
		Day:     g.DayNumber,
		Event:   "lynched",
		UserID:  &lid,
		Role:    lynched.Role,
		Message: fmt.Sprintf("%s oyundan çıxarıldı. (%s)", lynched.Name, roleLabel(lynched.Role)),
	})

	// Don çıxarılıbsa → yenidən təyin et
	g.reassignDonIfNeeded()
	return lynched
}

// ============================================================================
// QALİB YOXLAMA — plan §7
//
//	mafia sayı ≥ sakin sayı → mafia qazanır
//	mafia sayı = 0          → sakinlər qazanır
//
// ============================================================================
func (g *MafiaGame) checkWinner() (winner string, finished bool) {
	mafia := g.aliveMafiaCount()
	town := g.aliveTownCount()

	if mafia == 0 {
		return WinnerTown, true
	}
	if mafia >= town {
		return WinnerMafia, true
	}
	return "", false
}

// ============================================================================
// MƏRHƏLƏ KEÇİDİ — vaxt təyini
// ============================================================================

// setPhase — mərhələni dəyişir və phase_ends_at-i təyin edir.
func (g *MafiaGame) setPhase(phase string, durationSeconds int) {
	g.Phase = phase
	g.PhaseEndsAt = time.Now().UTC().Add(time.Duration(durationSeconds) * time.Second)
}

// phaseExpired — cari mərhələnin vaxtı bitibsə true.
func (g *MafiaGame) phaseExpired() bool {
	return time.Now().UTC().After(g.PhaseEndsAt)
}

// roleLabel — rol açarını oxunaqlı etiketə çevirir (chat elanları üçün).
func roleLabel(role string) string {
	switch role {
	case RoleDon:
		return "Don Mafia"
	case RoleMafia:
		return "Mafia"
	case RoleDetective:
		return "Dedektiv"
	case RoleCitizen:
		return "Sakin"
	default:
		return role
	}
}
