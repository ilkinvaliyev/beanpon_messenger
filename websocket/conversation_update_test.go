package websocket

import (
	"os"
	"testing"

	"beanpon_messenger/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ── C2 / DM-Q1 doğrulama testi (WS ikizi) ──────────────────────────────────
//
// `applyConversationMessageUpdateDB`, `handlers.applyConversationMessageUpdate`
// ile birebir aynı mantığı taşır (paketler arası ihraç etmemek için
// tekrarlanmıştır). Bu yol iOS istemcisinin ASIL gönderim yolu olduğu için
// ayrıca test edilir.
//
// CONV_TEST_DSN="host=/tmp port=5433 user=postgres dbname=bench" go test ./websocket/
func wsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("CONV_TEST_DSN")
	if dsn == "" {
		t.Skip("CONV_TEST_DSN yok — veritabanı testi atlandı")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("veritabanına bağlanılamadı: %v", err)
	}
	return db
}

func wsSeedConv(t *testing.T, db *gorm.DB, id, u1, u2 uint, status string, c1, c2, maxPending int, hasPrev bool) {
	t.Helper()
	err := db.Exec(`
        INSERT INTO conversations (id, user1_id, user2_id, status, user1_message_count,
                                   user2_message_count, max_pending_messages,
                                   total_messages_count, has_previous_conversation,
                                   first_message_at, last_message_at, deleted_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, NULL, NULL, NULL)
        ON CONFLICT (id) DO UPDATE SET
            user1_id = EXCLUDED.user1_id, user2_id = EXCLUDED.user2_id,
            status = EXCLUDED.status,
            user1_message_count = EXCLUDED.user1_message_count,
            user2_message_count = EXCLUDED.user2_message_count,
            max_pending_messages = EXCLUDED.max_pending_messages,
            total_messages_count = 0, has_previous_conversation = EXCLUDED.has_previous_conversation,
            first_message_at = NULL, last_message_at = NULL, deleted_at = NULL
    `, id, u1, u2, status, c1, c2, maxPending, hasPrev).Error
	if err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}
}

func TestApplyConversationMessageUpdateDB_Counters(t *testing.T) {
	db := wsTestDB(t)
	const id, u1, u2 = 990201, 5101, 5102
	wsSeedConv(t, db, id, u1, u2, "active", 4, 7, 3, true)

	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "active", HasPreviousConversation: true}
	if err := applyConversationMessageUpdateDB(db, &conv, u2); err != nil {
		t.Fatalf("güncelleme: %v", err)
	}

	var c1, c2v, total int
	row := db.Raw("SELECT user1_message_count, user2_message_count, total_messages_count FROM conversations WHERE id = ?", id).Row()
	if err := row.Scan(&c1, &c2v, &total); err != nil {
		t.Fatalf("okuma: %v", err)
	}
	if c1 != 4 || c2v != 8 || total != 1 {
		t.Fatalf("sayaçlar %d/%d/%d, beklenen 4/8/1", c1, c2v, total)
	}
	if conv.User2MessageCount != 8 || conv.Status != "active" {
		t.Fatalf("çağıranın kopyası tazelenmedi: %+v", conv)
	}
}

func TestApplyConversationMessageUpdateDB_PendingToActive(t *testing.T) {
	db := wsTestDB(t)
	const id, u1, u2 = 990202, 5103, 5104
	wsSeedConv(t, db, id, u1, u2, "pending", 2, 0, 3, false)

	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "pending"}
	if err := applyConversationMessageUpdateDB(db, &conv, u2); err != nil {
		t.Fatalf("güncelleme: %v", err)
	}
	var status string
	var hasPrev bool
	row := db.Raw("SELECT status, has_previous_conversation FROM conversations WHERE id = ?", id).Row()
	if err := row.Scan(&status, &hasPrev); err != nil {
		t.Fatalf("okuma: %v", err)
	}
	if status != "active" || !hasPrev {
		t.Fatalf("status=%q hasPrev=%v, beklenen active/true", status, hasPrev)
	}
	if conv.Status != "active" {
		t.Fatalf("çağıranın kopyasında status = %q", conv.Status)
	}
}

func TestApplyConversationMessageUpdateDB_PendingToRestricted(t *testing.T) {
	db := wsTestDB(t)
	const id, u1, u2 = 990203, 5105, 5106
	wsSeedConv(t, db, id, u1, u2, "pending", 3, 0, 3, false)

	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "pending", MaxPendingMessages: 3}
	if err := applyConversationMessageUpdateDB(db, &conv, u1); err != nil {
		t.Fatalf("güncelleme: %v", err)
	}
	var status string
	if err := db.Raw("SELECT status FROM conversations WHERE id = ?", id).Row().Scan(&status); err != nil {
		t.Fatalf("okuma: %v", err)
	}
	if status != "restricted" {
		t.Fatalf("status = %q, beklenen restricted", status)
	}
}

// Yumuşak silinmiş söhbət güncellenmemeli (ham SQL'de `deleted_at IS NULL`).
func TestApplyConversationMessageUpdateDB_SoftDeletedIgnored(t *testing.T) {
	db := wsTestDB(t)
	const id, u1, u2 = 990204, 5107, 5108
	wsSeedConv(t, db, id, u1, u2, "active", 4, 7, 3, true)
	if err := db.Exec("UPDATE conversations SET deleted_at = now() WHERE id = ?", id).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "active", HasPreviousConversation: true}
	if err := applyConversationMessageUpdateDB(db, &conv, u1); err != nil {
		t.Fatalf("hata dönmemeliydi: %v", err)
	}
	var c1, total int
	if err := db.Raw("SELECT user1_message_count, total_messages_count FROM conversations WHERE id = ?", id).
		Row().Scan(&c1, &total); err != nil {
		t.Fatalf("okuma: %v", err)
	}
	if c1 != 4 || total != 0 {
		t.Fatalf("silinmiş satırın sayaçları değişti: %d/%d, beklenen 4/0", c1, total)
	}
}
