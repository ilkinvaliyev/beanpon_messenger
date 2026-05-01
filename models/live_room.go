package models

import (
	"encoding/json"
	"time"
)

// LiveRoom - Laravel'deki 'live_rooms' tablosunun Go karşılığı (Sadece okuma amaçlı)
type LiveRoom struct {
	ID                 uint             `json:"id" gorm:"primaryKey;autoIncrement"`
	HostUserID         uint             `json:"host_user_id" gorm:"not null;index"`
	ChannelName        string           `json:"channel_name" gorm:"unique;not null"`
	Title              *string          `json:"title"`
	RoomType           string           `json:"room_type"` // audio, video, both
	Status             string           `json:"status"`    // waiting, live, ended
	HasPassword        bool             `json:"has_password"`
	HasBlocked         bool             `json:"has_blocked"`
	FilterGender       *string          `json:"filter_gender"`
	FilterMinAge       *int             `json:"filter_min_age"`
	FilterMaxAge       *int             `json:"filter_max_age"`
	FilterVerifiedOnly bool             `json:"filter_verified_only"`
	MaxBroadcasters    int              `json:"max_broadcasters"`
	PeakViewerCount    int              `json:"peak_viewer_count"`
	ActiveGame         *json.RawMessage `json:"active_game" gorm:"type:jsonb"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
	DeletedAt          *time.Time       `json:"deleted_at" gorm:"index"`
}

// LiveRoomParticipant - Laravel'deki 'live_room_participants' (Sadece okuma amaçlı)
type LiveRoomParticipant struct {
	ID          uint       `json:"id" gorm:"primaryKey;autoIncrement"`
	LiveRoomID  uint       `json:"live_room_id" gorm:"not null;index"`
	UserID      uint       `json:"user_id" gorm:"not null;index"`
	Role        string     `json:"role"`   // host, broadcaster, audience
	Status      string     `json:"status"` // pending, active, rejected, left, kicked
	AgoraUID    *uint      `json:"agora_uid"`
	RequestedAt *time.Time `json:"requested_at"`
	JoinedAt    *time.Time `json:"joined_at"`
	LeftAt      *time.Time `json:"left_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// LiveRoomMessage - BİZİM GOLANG'DA YÖNETECEĞİMİZ CHAT TABLOSU
type LiveRoomMessage struct {
	ID         uint      `json:"id" gorm:"primaryKey;autoIncrement"`
	LiveRoomID uint      `json:"live_room_id" gorm:"not null;index"`
	SenderID   uint      `json:"sender_id" gorm:"not null;index"`
	ReplyToID  *uint     `json:"reply_to_id" gorm:"index"`
	Text       string    `json:"text" gorm:"type:text;not null"`
	GifURL     *string   `json:"gif_url" gorm:"type:varchar(500)"`   // ← YENİ
	ImageURL   *string   `json:"image_url" gorm:"type:varchar(500)"` // ← YENİ
	CreatedAt  time.Time `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt  time.Time `json:"updated_at" gorm:"autoUpdateTime"`

	Sender User `json:"sender" gorm:"foreignKey:SenderID"`
}

type LiveRoomReaction struct {
	ID           uint      `json:"id" gorm:"primaryKey;autoIncrement"`
	LiveRoomID   uint      `json:"live_room_id" gorm:"not null;index"`
	ReactionName string    `json:"reaction_name" gorm:"not null"`
	Count        uint64    `json:"count" gorm:"default:0"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Reply preview üçün ayrı struct (JOIN-siz istifadə olunur)
type LiveRoomReplyPreview struct {
	ID           uint    `json:"id"`
	Text         string  `json:"text"`
	SenderID     uint    `json:"sender_id"`
	SenderName   string  `json:"sender_name"`
	SenderAvatar *string `json:"sender_avatar"`
}

// Tablo isimlerini GORM için belirtiyoruz (Laravel ile uyumlu olsun diye)
func (LiveRoom) TableName() string {
	return "live_rooms"
}
func (LiveRoomParticipant) TableName() string {
	return "live_room_participants"
}
func (LiveRoomMessage) TableName() string {
	return "live_room_messages"
}

func (LiveRoomReaction) TableName() string {
	return "live_room_reactions"
}
