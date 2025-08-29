package models

import (
	"github.com/google/uuid"
	"gorm.io/gorm"
	"time"
)

// Message mesaj tablosu
type Message struct {
	ID               string  `json:"id" gorm:"type:uuid;primary_key"`
	SenderID         uint    `json:"sender_id" gorm:"not null;index"`
	ReceiverID       uint    `json:"receiver_id" gorm:"not null;index"`
	ReplyToMessageID *string `json:"reply_to_message_id" gorm:"type:uuid;index"`
	EncryptedText    string  `json:"encrypted_text" gorm:"type:text;not null"`
	EncryptedAESKey  *string `json:"encrypted_aes_key" gorm:"type:text"`
	//Type             string     `json:"type" gorm:"default:'text'"`
	IsEdited            bool           `json:"is_edited" gorm:"default:false"`
	IsDeletedBySender   bool           `json:"is_deleted_by_sender" gorm:"default:false"`
	IsDeletedByReceiver bool           `json:"is_deleted_by_receiver" gorm:"default:false"`
	Delivered           bool           `json:"delivered" gorm:"default:false"`
	Read                bool           `json:"read" gorm:"default:false"`
	ReadAt              *time.Time     `json:"read_at"`
	SenderReaction      *string        `json:"sender_reaction" gorm:"type:varchar(10)"`
	ReceiverReaction    *string        `json:"receiver_reaction" gorm:"type:varchar(10)"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	DeletedAt           gorm.DeletedAt `json:"deleted_at" gorm:"index"`

	// İlişkiler
	Sender         User     `json:"sender" gorm:"foreignKey:SenderID"`
	Receiver       User     `json:"receiver" gorm:"foreignKey:ReceiverID"`
	ReplyToMessage *Message `json:"reply_to_message,omitempty" gorm:"foreignKey:ReplyToMessageID"`
}

// MessageEdit mesaj düzenleme geçmişi
type MessageEdit struct {
	ID               string    `json:"id" gorm:"type:uuid;primary_key"`
	MessageID        string    `json:"message_id" gorm:"type:uuid;not null;index"`
	EncryptedOldText string    `json:"encrypted_old_text" gorm:"type:text;not null"`
	EncryptedAESKey  *string   `json:"encrypted_aes_key" gorm:"type:text"`
	EditedAt         time.Time `json:"edited_at" gorm:"default:CURRENT_TIMESTAMP"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	// İlişki
	Message Message `json:"message" gorm:"foreignKey:MessageID;constraint:OnDelete:CASCADE"`
}

// BeforeCreate hook - UUID oluştur
func (m *Message) BeforeCreate(tx *gorm.DB) error {
	if m.ID == "" {
		m.ID = uuid.New().String()
	}
	return nil
}

// BeforeCreate hook - UUID oluştur
func (me *MessageEdit) BeforeCreate(tx *gorm.DB) error {
	if me.ID == "" {
		me.ID = uuid.New().String()
	}
	return nil
}

// MessageResponse API response için
type MessageResponse struct {
	ID         string     `json:"id"`
	SenderID   uint       `json:"sender_id"`
	ReceiverID uint       `json:"receiver_id"`
	Text       string     `json:"text"` // Çözülmüş metin
	IsEdited   bool       `json:"is_edited"`
	Delivered  bool       `json:"delivered"`
	Read       bool       `json:"read"`
	ReadAt     *time.Time `json:"read_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// SendMessageRequest mesaj gönderme isteği
type SendMessageRequest struct {
	ReceiverID uint   `json:"receiver_id" binding:"required"`
	Text       string `json:"text" binding:"required,min=1,max=5000"`
	//Type             string  `json:"type,omitempty"` // Bu satırı ekle
	ReplyToMessageID *string `json:"reply_to_message_id,omitempty"`
}
