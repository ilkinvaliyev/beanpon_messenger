package models

import (
	"gorm.io/gorm"
	"time"
)

// Conversation model - models.go dosyasının sonuna ekle
type Conversation struct {
	ID                      uint           `json:"id" gorm:"primaryKey"`
	User1ID                 uint           `json:"user1_id" gorm:"not null;index"`
	User2ID                 uint           `json:"user2_id" gorm:"not null;index"`
	Status                  string         `json:"status" gorm:"type:varchar(20);default:'pending';check:status IN ('pending','active','restricted')"`
	Type                    string         `json:"type" gorm:"type:varchar(20);default:'request_based';check:type IN ('follow_based','request_based')"`
	User1MessageCount       int            `json:"user1_message_count" gorm:"default:0"`
	User2MessageCount       int            `json:"user2_message_count" gorm:"default:0"`
	MaxPendingMessages      int            `json:"max_pending_messages" gorm:"default:3"`
	User1FollowsUser2       bool           `json:"user1_follows_user2" gorm:"default:false"`
	User2FollowsUser1       bool           `json:"user2_follows_user1" gorm:"default:false"`
	MutualFollow            bool           `json:"mutual_follow" gorm:"default:false"`
	HasPreviousConversation bool           `json:"has_previous_conversation" gorm:"default:false"`
	FirstMessageAt          *time.Time     `json:"first_message_at"`
	LastMessageAt           *time.Time     `json:"last_message_at"`
	User1Muted              bool           `json:"user1_muted" gorm:"default:false"`
	User2Muted              bool           `json:"user2_muted" gorm:"default:false"`
	User1MutedAt            *time.Time     `json:"user1_muted_at"`
	User2MutedAt            *time.Time     `json:"user2_muted_at"`
	User1Restricted         bool           `json:"user1_restricted" gorm:"default:false"`
	User2Restricted         bool           `json:"user2_restricted" gorm:"default:false"`
	RestrictionReason       *string        `json:"restriction_reason" gorm:"type:text"`
	TotalMessagesCount      int            `json:"total_messages_count" gorm:"default:0"`
	StatusChangedAt         *time.Time     `json:"status_changed_at"`
	FollowHistory           *string        `json:"follow_history" gorm:"type:jsonb"` // PostgreSQL için jsonb
	CreatedAt               time.Time      `json:"created_at"`
	UpdatedAt               time.Time      `json:"updated_at"`
	DeletedAt               gorm.DeletedAt `json:"deleted_at" gorm:"index"`

	// İlişkiler
	User1 User `json:"user1" gorm:"foreignKey:User1ID"`
	User2 User `json:"user2" gorm:"foreignKey:User2ID"`
}

// ConversationResponse API response için
type ConversationResponse struct {
	ID                      uint       `json:"id"`
	OtherUserID             uint       `json:"other_user_id"`
	Status                  string     `json:"status"`
	Type                    string     `json:"type"`
	MyMessageCount          int        `json:"my_message_count"`
	OtherMessageCount       int        `json:"other_message_count"`
	IsMutedByMe             bool       `json:"is_muted_by_me"`
	IsRestrictedForMe       bool       `json:"is_restricted_for_me"`
	CanSendMessage          bool       `json:"can_send_message"`
	MaxPendingMessages      int        `json:"max_pending_messages"`
	HasPreviousConversation bool       `json:"has_previous_conversation"`
	LastMessageAt           *time.Time `json:"last_message_at"`
	CreatedAt               time.Time  `json:"created_at"`
}
