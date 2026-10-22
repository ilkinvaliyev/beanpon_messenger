package watch

import (
	"log"
	"time"
)

// sweep — every 30 s: expire join requests, end rooms nobody is in, remind
// hosts of scheduled rooms, drop schedules nobody opened, close polls.
func (m *Module) sweep(now time.Time) {
	cfg := m.settings.get()
	if !cfg.loaded {
		return
	}

	// 1. Expired join requests.
	var expired []struct {
		RoomID int64
		UserID int64
	}
	m.db.Raw(`UPDATE watch_room_requests SET status = 'expired', updated_at = NOW()
		WHERE status = 'pending' AND expires_at IS NOT NULL AND expires_at < NOW()
		RETURNING room_id, user_id`).Scan(&expired)
	for _, e := range expired {
		if r := m.hub.loaded(e.RoomID); r != nil {
			e := e
			r.locked(func() {
				if !r.gone {
					r.toControllers("request_resolved", map[string]any{"user_id": e.UserID, "status": "expired"})
				}
			})
		}
	}

	// 2. Open rooms with nobody in them for empty_room_minutes.
	var empty []int64
	m.db.Raw(`SELECT id FROM watch_rooms
		WHERE status IN ('lobby', 'live')
		  AND COALESCE(last_active_at, created_at) < NOW() - make_interval(mins => ?)
		LIMIT 200`, cfg.s.EmptyRoomMinutes).Scan(&empty)
	window := time.Duration(cfg.s.EmptyRoomMinutes) * time.Minute
	for _, id := range empty {
		_ = m.withRoom(id, func(r *liveRoom) error {
			if len(r.clients) > 0 || time.Since(r.lastTouch) < window {
				return nil
			}
			return m.endRoomLocked(r, "empty")
		})
	}

	// 3. Scheduled rooms whose time has come: remind the host once.
	var due []struct {
		ID     int64
		HostID int64
		Name   string
	}
	m.db.Raw(`UPDATE watch_rooms SET open_reminder_sent_at = NOW()
		WHERE status = 'scheduled' AND scheduled_at <= NOW() AND open_reminder_sent_at IS NULL
		RETURNING id, host_id, name`).Scan(&due)
	for _, d := range due {
		d := d
		go m.push(d.HostID, "watch_open_reminder", map[string]string{"room": d.Name},
			map[string]string{"room_id": itoa(d.ID)})
	}

	// 4. Scheduled rooms nobody opened within the grace period.
	var stale []int64
	m.db.Raw(`SELECT id FROM watch_rooms
		WHERE status = 'scheduled' AND scheduled_at < NOW() - make_interval(mins => ?)
		LIMIT 200`, cfg.s.ScheduledGraceMinutes).Scan(&stale)
	for _, id := range stale {
		_ = m.withRoom(id, func(r *liveRoom) error {
			if r.row.Status != "scheduled" {
				return nil
			}
			return m.endRoomLocked(r, "expired")
		})
	}

	// 5. Polls past closes_at.
	var polls []struct {
		ID     int64
		RoomID int64
	}
	m.db.Raw(`SELECT id, room_id FROM watch_room_polls
		WHERE closed_at IS NULL AND closes_at IS NOT NULL AND closes_at <= NOW()
		ORDER BY id LIMIT 200`).Scan(&polls)
	for _, p := range polls {
		p := p
		err := m.withRoom(p.RoomID, func(r *liveRoom) error {
			if r.poll != nil && r.poll.ID == p.ID {
				m.finishPollLocked(r, 0)
				return nil
			}
			// Not the room's open poll (stale row) — just close it.
			m.db.Exec(`UPDATE watch_room_polls SET closed_at = NOW(), updated_at = NOW() WHERE id = ? AND closed_at IS NULL`, p.ID)
			return nil
		})
		if err != nil {
			log.Printf("[watch] sweep poll %d: %v", p.ID, err)
			m.db.Exec(`UPDATE watch_room_polls SET closed_at = NOW(), updated_at = NOW() WHERE id = ? AND closed_at IS NULL`, p.ID)
		}
	}
}
