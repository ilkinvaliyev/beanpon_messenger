package websocket

import (
	"encoding/json"
	"time"
)

// ============================================================================
// MAFIA OYUNU — Axın idarəsi (mərhələ keçidi + WS event handle + timer)
//
// advancePhase — cari mərhələni bitirib növbətiyə keçirir, lazımi
// hesablamaları (gecə/səs resolve) edir, broadcast göndərir, qalib yoxlayır.
// handleMafiaEvent — client-dən gələn mafia event-lərini emal edir.
// ============================================================================

// advancePhase — oyunu növbəti mərhələyə keçirir. roomID üçün cari game
// vəziyyətini götürüb dəyişir və saxlayır. Mutex sahibi çağıran tərəf
// OLMAMALIDIR (DB + broadcast içəridə edilir).
func (h *LiveHub) advancePhase(roomID uint) {
	game, ok := loadGame(roomID)
	if !ok {
		return
	}

	switch game.Phase {

	case MafiaPhaseIntro:
		// Kart mərhələsi bitdi → gecəyə keç
		h.enterNight(roomID, game)

	case MafiaPhaseNight:
		// Gecə bitdi → hərəkətləri hesabla, sabahı elan et
		game.resolveNight()
		h.announceNight(roomID, game)
		if h.checkAndFinish(roomID, game) {
			return
		}
		// Gündüz müzakirəyə keç (dinamik timer)
		h.enterDay(roomID, game)

	case MafiaPhaseDay:
		// Müzakirə bitdi → ilk səsverməyə keç
		game.resetVotes()
		game.OnTrial = nil
		game.DefenseIndex = 0
		secs := mafiaVoteSeconds
		game.setPhase(MafiaPhaseVote, secs)
		saveGame(roomID, game)
		h.broadcastPublicState(roomID, game, "mafia_phase_changed")
		h.systemChatMessage(roomID, "Səsvermə başladı. Mahkəməyə kimi göndərəcəyinizi seçin.")

	case MafiaPhaseVote:
		// İlk səsvermə bitdi → mahkəmədəkiləri təyin et
		game.resolveFirstVote()
		if len(game.OnTrial) == 0 {
			// Heç kim mahkəməyə düşmədi → birbaşa gecə
			h.systemChatMessage(roomID, "Heç kim mahkəməyə düşmədi. Gecə başlayır.")
			h.enterNight(roomID, game)
			return
		}
		// Müdafiə mərhələsi (ilk mahkəmədəkidən başla)
		h.enterDefense(roomID, game)

	case MafiaPhaseDefense:
		// Cari müdafiə bitdi → növbəti mahkəmədəkiyə, ya son səsverməyə
		game.DefenseIndex++
		if game.DefenseIndex < len(game.OnTrial) {
			h.enterDefense(roomID, game)
		} else {
			// Bütün müdafiələr bitdi → son səsvermə
			game.resetVotes()
			game.setPhase(MafiaPhaseFinalVote, mafiaFinalVoteSeconds)
			saveGame(roomID, game)
			h.broadcastPublicState(roomID, game, "mafia_phase_changed")
			h.systemChatMessage(roomID, "Son səsvermə. Mahkəmədəkilərdən birini seçin.")
		}

	case MafiaPhaseFinalVote:
		// Son səsvermə bitdi → ən çoxlu çıxır
		lynched := game.resolveFinalVote()
		game.OnTrial = nil
		game.DefenseIndex = 0
		if lynched != nil {
			// Çıxış mərhələsi (30s reaksiya) — kartı açıq
			game.setPhase(MafiaPhaseExecution, mafiaExecutionSeconds)
			saveGame(roomID, game)
			h.broadcastPublicState(roomID, game, "mafia_phase_changed")
			h.announceHistoryTail(roomID, game)
			if h.checkAndFinish(roomID, game) {
				return
			}
		} else {
			// Bərabərlik → birbaşa gecə
			saveGame(roomID, game)
			h.announceHistoryTail(roomID, game)
			h.enterNight(roomID, game)
		}

	case MafiaPhaseExecution:
		// Reaksiya bitdi → gecəyə keç
		h.enterNight(roomID, game)
	}
}

// enterNight — gecə mərhələsinə keçir, səsləri bağla, Don/Dedektivə hərəkət
// imkanı ver.
func (h *LiveHub) enterNight(roomID uint, game *MafiaGame) {
	game.DayNumber++
	game.NightActions = MafiaNightActions{}
	game.resetVotes()
	game.OnTrial = nil
	game.DefenseIndex = 0
	game.setPhase(MafiaPhaseNight, mafiaNightSeconds)
	saveGame(roomID, game)

	// Səsləri bağla (hamı) — sistem komandası
	h.broadcastMute(roomID, true)
	h.broadcastPublicState(roomID, game, "mafia_phase_changed")
	h.systemChatMessage(roomID, "Gecə oldu. Şəhər yatır...")
}

// enterDay — gündüz müzakirəyə keçir, səsləri aç, dinamik timer.
func (h *LiveHub) enterDay(roomID uint, game *MafiaGame) {
	aliveCount := len(game.alivePlayers())
	secs := dayDiscussionSeconds(aliveCount)
	game.resetVotes()
	game.OnTrial = nil
	game.DefenseIndex = 0
	game.setPhase(MafiaPhaseDay, secs)
	saveGame(roomID, game)

	// Səsləri aç (hamı)
	h.broadcastMute(roomID, false)
	h.broadcastPublicState(roomID, game, "mafia_phase_changed")
	h.systemChatMessage(roomID, "Sabah oldu. Müzakirə başladı.")
}

// enterDefense — cari müdafiə növbəsi: yalnız o oyunçunun səsi açıq, qalan lal.
func (h *LiveHub) enterDefense(roomID uint, game *MafiaGame) {
	game.setPhase(MafiaPhaseDefense, mafiaDefenseSeconds)
	saveGame(roomID, game)

	defenderID := game.OnTrial[game.DefenseIndex]
	// Hamını lal et, yalnız müdafiə edəni aç
	h.broadcastMute(roomID, true)
	h.unmuteSingle(roomID, defenderID)

	data, _ := json.Marshal(map[string]interface{}{
		"user_id":       defenderID,
		"phase_ends_at": game.PhaseEndsAt,
		"defense_index": game.DefenseIndex,
		"total":         len(game.OnTrial),
	})
	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "mafia_defense_turn",
		"room_id": roomID,
		"data":    json.RawMessage(data),
	})
	h.broadcastRaw(roomID, payload)

	if defender := game.findPlayer(defenderID); defender != nil {
		h.systemChatMessage(roomID, defender.Name+" müdafiə edir (1 dəqiqə).")
	}
}

// announceNight — gecə tarixçəsindəki yeni qeydləri chat-a yazır + state yay.
func (h *LiveHub) announceNight(roomID uint, game *MafiaGame) {
	saveGame(roomID, game)
	h.broadcastPublicState(roomID, game, "mafia_night_result")
	h.announceHistoryTail(roomID, game)
}

// announceHistoryTail — son hadisə(lər)i chat-a sistem mesajı kimi yazır.
// Cari günə aid bütün qeydləri elan edir.
func (h *LiveHub) announceHistoryTail(roomID uint, game *MafiaGame) {
	for i := len(game.History) - 1; i >= 0; i-- {
		entry := game.History[i]
		if entry.Day != game.DayNumber {
			break
		}
		if entry.Message != "" {
			h.systemChatMessage(roomID, entry.Message)
		}
	}
}

// checkAndFinish — qalib varsa oyunu bitirir. true → oyun bitdi.
func (h *LiveHub) checkAndFinish(roomID uint, game *MafiaGame) bool {
	winner, finished := game.checkWinner()
	if !finished {
		return false
	}
	game.Winner = winner
	game.setPhase(MafiaPhaseEnded, 0)
	saveGame(roomID, game)

	// Oyun bitdi — bütün rollar açıq
	h.broadcastPublicState(roomID, game, "mafia_game_over")

	msg := "Sakinlər qazandı! 🎉"
	if winner == WinnerMafia {
		msg = "Mafia qazandı! 🔪"
	}
	h.systemChatMessage(roomID, msg)

	// Səsləri aç (oyun bitdi, hamı danışsın)
	h.broadcastMute(roomID, false)

	// active_game-i təmizlə
	clearGame(roomID)
	return true
}

// ============================================================================
// Səs idarəsi — sistem hamını lal/aç edir (plan §2, §5)
// Flutter tərəfi bu event-i alıb Agora mikrofon idarəsini edir.
// ============================================================================

func (h *LiveHub) broadcastMute(roomID uint, muted bool) {
	data, _ := json.Marshal(map[string]interface{}{"muted": muted, "scope": "all"})
	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "mafia_force_mute",
		"room_id": roomID,
		"data":    json.RawMessage(data),
	})
	h.broadcastRaw(roomID, payload)
}

func (h *LiveHub) unmuteSingle(roomID, userID uint) {
	data, _ := json.Marshal(map[string]interface{}{"muted": false, "user_id": userID})
	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "mafia_force_mute",
		"room_id": roomID,
		"data":    json.RawMessage(data),
	})
	h.broadcastRaw(roomID, payload)
}

// ============================================================================
// WS event handle — client → server
// live_hub.handleEvent içindən çağrılır.
// ============================================================================

func (h *LiveHub) handleMafiaEvent(event *LiveMessageEvent) {
	game, ok := loadGame(event.RoomID)
	if !ok {
		return
	}

	switch event.Type {

	case "mafia_ready":
		// "Hazırım" — intro mərhələsində
		if game.Phase != MafiaPhaseIntro {
			return
		}
		p := game.findPlayer(event.SenderID)
		if p == nil || !p.Alive {
			return
		}
		p.IsReady = true
		saveGame(event.RoomID, game)
		h.broadcastPublicState(event.RoomID, game, "mafia_phase_changed")
		// Hamı hazırsa erkən gecəyə keç
		if game.allReady() {
			h.enterNight(event.RoomID, game)
		}

	case "mafia_night_action":
		// Don vurur / Dedektiv seçir
		if game.Phase != MafiaPhaseNight {
			return
		}
		actor := game.findPlayer(event.SenderID)
		if actor == nil || !actor.Alive {
			return
		}
		var body struct {
			Action   string `json:"action"`
			TargetID uint   `json:"target_id"`
		}
		if err := json.Unmarshal(event.Data, &body); err != nil {
			return
		}
		// Hədəf özü ola bilməz, sağ olmalıdır
		if body.TargetID == event.SenderID {
			return
		}
		target := game.findPlayer(body.TargetID)
		if target == nil || !target.Alive {
			return
		}

		switch {
		case actor.Role == RoleDon && body.Action == "kill":
			game.NightActions.DonTarget = &body.TargetID
		case actor.Role == RoleDetective && body.Action == "check":
			game.NightActions.DetectiveTarget = &body.TargetID
		default:
			return // bu rol bu hərəkəti edə bilməz
		}
		saveGame(event.RoomID, game)

		// Actor-a ack (UI üçün)
		ack, _ := json.Marshal(map[string]interface{}{
			"type":    "mafia_action_ack",
			"room_id": event.RoomID,
			"data": map[string]interface{}{
				"action":    body.Action,
				"target_id": body.TargetID,
			},
		})
		h.sendToUser(event.RoomID, event.SenderID, ack)

		// Don + Dedektiv (sağ) ikisi də hərəkət edibsə erkən resolve
		if h.allNightActionsDone(game) {
			game.resolveNight()
			h.announceNight(event.RoomID, game)
			if h.checkAndFinish(event.RoomID, game) {
				return
			}
			h.enterDay(event.RoomID, game)
		}

	case "mafia_vote":
		// Səsvermə (day_voting və ya final_voting)
		if game.Phase != MafiaPhaseVote && game.Phase != MafiaPhaseFinalVote {
			return
		}
		voter := game.findPlayer(event.SenderID)
		if voter == nil || !voter.Alive {
			return
		}
		var body struct {
			TargetID uint `json:"target_id"`
		}
		if err := json.Unmarshal(event.Data, &body); err != nil {
			return
		}
		if body.TargetID == event.SenderID {
			return
		}
		target := game.findPlayer(body.TargetID)
		if target == nil || !target.Alive {
			return
		}
		// Final səsvermədə yalnız mahkəmədəkilərə səs verilə bilər
		if game.Phase == MafiaPhaseFinalVote {
			onTrial := false
			for _, id := range game.OnTrial {
				if id == body.TargetID {
					onTrial = true
					break
				}
			}
			if !onTrial {
				return
			}
		}
		game.Votes[event.SenderID] = body.TargetID
		voter.HasVoted = true
		saveGame(event.RoomID, game)

		// Səsləri hamıya yay (kim-kimə — açıq, plan §5.2)
		h.broadcastPublicState(event.RoomID, game, "mafia_vote_update")

		// Hamı səs veribsə erkən keç
		if game.allVoted() {
			h.advancePhase(event.RoomID)
		}

	case "mafia_defense_end":
		// "Bitir" — yalnız cari müdafiə edən (plan §5.3)
		if game.Phase != MafiaPhaseDefense {
			return
		}
		if len(game.OnTrial) == 0 || game.DefenseIndex >= len(game.OnTrial) {
			return
		}
		if game.OnTrial[game.DefenseIndex] != event.SenderID {
			return // yalnız sözü olan bitirə bilər
		}
		h.advancePhase(event.RoomID)

	case "mafia_cancel":
		// Host oyunu dayandırır
		h.mu.RLock()
		roomClients := h.rooms[event.RoomID]
		var sender *LiveRoomClient
		if roomClients != nil {
			sender = roomClients[event.SenderID]
		}
		h.mu.RUnlock()
		if sender == nil || sender.Role != "host" {
			return
		}
		clearGame(event.RoomID)
		h.broadcastMute(event.RoomID, false)
		payload, _ := json.Marshal(map[string]interface{}{
			"type":    "mafia_cancelled",
			"room_id": event.RoomID,
			"data":    map[string]interface{}{},
		})
		h.broadcastRaw(event.RoomID, payload)
		h.systemChatMessage(event.RoomID, "Mafia oyunu dayandırıldı.")
	}
}

// allNightActionsDone — sağ Don və sağ Dedektiv hərəkət edibsə true.
// (Hər ikisi yoxdursa olanların hamısı edibsə yenə true.)
func (h *LiveHub) allNightActionsDone(game *MafiaGame) bool {
	hasDon, hasDetective := false, false
	for _, p := range game.Players {
		if !p.Alive {
			continue
		}
		if p.Role == RoleDon {
			hasDon = true
		}
		if p.Role == RoleDetective {
			hasDetective = true
		}
	}
	if hasDon && game.NightActions.DonTarget == nil {
		return false
	}
	if hasDetective && game.NightActions.DetectiveTarget == nil {
		return false
	}
	return true
}

// ============================================================================
// TIMER — mərkəzi goroutine. Hər saniyə vaxtı bitən mafia otaqlarını yoxlayır.
// main.go-da `go liveHub.RunMafiaTimer()` ilə işə salınır.
// ============================================================================
func (h *LiveHub) RunMafiaTimer() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		// Aktiv mafia otaqlarını tap (online client-i olanlar)
		h.mu.RLock()
		roomIDs := make([]uint, 0, len(h.rooms))
		for rid := range h.rooms {
			roomIDs = append(roomIDs, rid)
		}
		h.mu.RUnlock()

		for _, rid := range roomIDs {
			game, ok := loadGame(rid)
			if !ok || game.Phase == MafiaPhaseEnded {
				continue
			}
			if game.phaseExpired() {
				h.advancePhase(rid)
			}
		}
	}
}
