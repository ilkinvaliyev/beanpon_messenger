package models

import "time"

// LiveRoomQuestion - Laravel'deki 'live_room_questions' tablosunun Go karşılığı (Sadece okuma + shown_count update)
type LiveRoomQuestion struct {
	ID         uint      `json:"id" gorm:"primaryKey;autoIncrement"`
	Text       string    `json:"text" gorm:"type:text;not null"`
	Lang       string    `json:"lang" gorm:"type:varchar(8);default:'az'"`
	Type       string    `json:"type" gorm:"type:varchar(32);default:'normal'"`
	ShownCount uint64    `json:"shown_count" gorm:"default:0"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (LiveRoomQuestion) TableName() string {
	return "live_room_questions"
}

// LiveRoomQuestionUsage - Laravel'deki 'live_room_question_usages' tablosunun Go karşılığı
type LiveRoomQuestionUsage struct {
	ID            uint      `json:"id" gorm:"primaryKey;autoIncrement"`
	LiveRoomID    uint      `json:"live_room_id" gorm:"not null;index"`
	QuestionID    uint      `json:"question_id" gorm:"not null;index"`
	AskedByUserID *uint     `json:"asked_by_user_id"`
	ShownAt       time.Time `json:"shown_at"`
	CreatedAt     time.Time `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt     time.Time `json:"updated_at" gorm:"autoUpdateTime"`
}

func (LiveRoomQuestionUsage) TableName() string {
	return "live_room_question_usages"
}
