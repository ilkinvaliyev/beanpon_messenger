package handlers

import (
	"os"
	"testing"

	"beanpon_messenger/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ── C2 / DM-Q1 doğrulama testi ─────────────────────────────────────────────
//
// `applyConversationMessageUpdate` UPDATE + SELECT ikilisinden tek
// `UPDATE ... RETURNING`e geçti. Bu test sayaç artışının, durum geçişlerinin
// ve "satır yok" davranışının GERÇEK PostgreSQL üzerinde aynı kaldığını
// doğrular.
//
// Çalıştırmak için:  CONV_TEST_DSN="host=/tmp port=5433 user=postgres dbname=bench" go test ./handlers/
// DSN verilmezse test atlanır (CI'da veritabanı olmayabilir).
func testDB(t *testing.T) *gorm.DB {
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

func seedConv(t *testing.T, db *gorm.DB, id, u1, u2 uint, status string, c1, c2, maxPending int, hasPrev bool) {
	t.Helper()
	err := db.Exec(`
        INSERT INTO conversations (id, user1_id, user2_id, status, user1_message_count,
                                   user2_message_count, max_pending_messages,
                                   total_messages_count, has_previous_conversation,
                                   first_message_at, last_message_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, NULL, NULL)
        ON CONFLICT (id) DO UPDATE SET
            user1_id = EXCLUDED.user1_id, user2_id = EXCLUDED.user2_id,
            status = EXCLUDED.status,
            user1_message_count = EXCLUDED.user1_message_count,
            user2_message_count = EXCLUDED.user2_message_count,
            max_pending_messages = EXCLUDED.max_pending_messages,
            total_messages_count = 0, has_previous_conversation = EXCLUDED.has_previous_conversation,
            first_message_at = NULL, last_message_at = NULL
    `, id, u1, u2, status, c1, c2, maxPending, hasPrev).Error
	if err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}
}

func readConv(t *testing.T, db *gorm.DB, id uint) models.Conversation {
	t.Helper()
	var c models.Conversation
	if err := db.Where("id = ?", id).First(&c).Error; err != nil {
		t.Fatalf("okuma başarısız: %v", err)
	}
	return c
}

// Sayaç artışı + first_message_at yalnız ilk kez + total artışı.
func TestApplyConversationMessageUpdate_Counters(t *testing.T) {
	db := testDB(t)
	const id, u1, u2 = 990101, 5001, 5002
	seedConv(t, db, id, u1, u2, "active", 4, 7, 3, true)

	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "active", HasPreviousConversation: true}
	applied, err := applyConversationMessageUpdate(db, &conv, u1)
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}

	got := readConv(t, db, id)
	if got.User1MessageCount != 5 || got.User2MessageCount != 7 {
		t.Fatalf("sayaçlar: %d/%d, beklenen 5/7", got.User1MessageCount, got.User2MessageCount)
	}
	if got.TotalMessagesCount != 1 {
		t.Fatalf("total = %d, beklenen 1", got.TotalMessagesCount)
	}
	if got.FirstMessageAt == nil || got.LastMessageAt == nil {
		t.Fatal("first/last_message_at yazılmadı")
	}
	first := *got.FirstMessageAt

	// İkinci mesaj: first_message_at DEĞİŞMEMELİ (COALESCE), sayaç artmalı.
	conv2 := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "active", HasPreviousConversation: true}
	if _, err := applyConversationMessageUpdate(db, &conv2, u2); err != nil {
		t.Fatalf("ikinci güncelleme: %v", err)
	}
	got2 := readConv(t, db, id)
	if got2.User1MessageCount != 5 || got2.User2MessageCount != 8 {
		t.Fatalf("sayaçlar: %d/%d, beklenen 5/8", got2.User1MessageCount, got2.User2MessageCount)
	}
	if !got2.FirstMessageAt.Equal(first) {
		t.Fatal("first_message_at ikinci mesajda değişti (COALESCE bozuk)")
	}
	// Çağıranın kopyası da tazelenmiş olmalı (push kapısı bunu okuyor — Issue 10).
	if conv2.Status != "active" || conv2.User2MessageCount != 8 {
		t.Fatalf("çağıranın kopyası tazelenmedi: %+v", conv2)
	}
}

// pending → active geçişi (her iki taraf da yazdıysa).
func TestApplyConversationMessageUpdate_PendingToActive(t *testing.T) {
	db := testDB(t)
	const id, u1, u2 = 990102, 5003, 5004
	seedConv(t, db, id, u1, u2, "pending", 2, 0, 3, false)

	// user2 ilk kez cevap veriyor → iki sayaç da > 0 → active.
	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "pending"}
	if _, err := applyConversationMessageUpdate(db, &conv, u2); err != nil {
		t.Fatalf("güncelleme: %v", err)
	}
	got := readConv(t, db, id)
	if got.Status != "active" {
		t.Fatalf("status = %q, beklenen active", got.Status)
	}
	if !got.HasPreviousConversation {
		t.Fatal("has_previous_conversation kalkmadı")
	}
	if conv.Status != "active" {
		t.Fatalf("çağıranın kopyasında status = %q", conv.Status)
	}
}

// pending → restricted (tek taraflı limit aşımı).
func TestApplyConversationMessageUpdate_PendingToRestricted(t *testing.T) {
	db := testDB(t)
	const id, u1, u2 = 990103, 5005, 5006
	seedConv(t, db, id, u1, u2, "pending", 3, 0, 3, false)

	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "pending", MaxPendingMessages: 3}
	if _, err := applyConversationMessageUpdate(db, &conv, u1); err != nil {
		t.Fatalf("güncelleme: %v", err)
	}
	got := readConv(t, db, id)
	if got.Status != "restricted" {
		t.Fatalf("status = %q, beklenen restricted", got.Status)
	}
	if conv.Status != "restricted" {
		t.Fatalf("çağıranın kopyasında status = %q", conv.Status)
	}
}

// Limit AŞILMADIYSA pending kalmalı.
func TestApplyConversationMessageUpdate_StaysPending(t *testing.T) {
	db := testDB(t)
	const id, u1, u2 = 990104, 5007, 5008
	seedConv(t, db, id, u1, u2, "pending", 1, 0, 3, false)

	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "pending", MaxPendingMessages: 3}
	if _, err := applyConversationMessageUpdate(db, &conv, u1); err != nil {
		t.Fatalf("güncelleme: %v", err)
	}
	if got := readConv(t, db, id); got.Status != "pending" {
		t.Fatalf("status = %q, beklenen pending", got.Status)
	}
}

// Satır yoksa: hata DEĞİL, `applied=false`. Çağıran eski yola (getOrCreate) düşer.
func TestApplyConversationMessageUpdate_MissingRow(t *testing.T) {
	db := testDB(t)
	db.Exec("DELETE FROM conversations WHERE id = ?", 990199)

	conv := models.Conversation{ID: 990199, User1ID: 1, User2ID: 2}
	applied, err := applyConversationMessageUpdate(db, &conv, 1)
	if err != nil {
		t.Fatalf("hata dönmemeliydi: %v", err)
	}
	if applied {
		t.Fatal("olmayan satır için applied=true döndü")
	}
}

// YUMUŞAK SİLİNMİŞ (soft-deleted) söhbət GÜNCELLENMEMELİ.
//
// Bu test bir regresyonu kilitliyor: ham SQL'e geçerken GORM'un `models.
// Conversation` için otomatik eklediği `deleted_at IS NULL` süzgeci kaybolmuştu.
// Eski kod (`db.Model(&models.Conversation{}).Where("id = ?")`) o süzgeci
// içeriyordu; yeni ham UPDATE'e elle eklendi.
func TestApplyConversationMessageUpdate_SoftDeletedIgnored(t *testing.T) {
	db := testDB(t)
	const id, u1, u2 = 990105, 5009, 5010
	seedConv(t, db, id, u1, u2, "active", 4, 7, 3, true)
	if err := db.Exec("UPDATE conversations SET deleted_at = now() WHERE id = ?", id).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	conv := models.Conversation{ID: id, User1ID: u1, User2ID: u2, Status: "active", HasPreviousConversation: true}
	applied, err := applyConversationMessageUpdate(db, &conv, u1)
	if err != nil {
		t.Fatalf("hata dönmemeliydi: %v", err)
	}
	if applied {
		t.Fatal("silinmiş söhbət güncellendi (deleted_at süzgeci kayıp)")
	}

	var c1, total int
	row := db.Raw("SELECT user1_message_count, total_messages_count FROM conversations WHERE id = ?", id).Row()
	if err := row.Scan(&c1, &total); err != nil {
		t.Fatalf("okuma: %v", err)
	}
	if c1 != 4 || total != 0 {
		t.Fatalf("silinmiş satırın sayaçları değişti: %d/%d, beklenen 4/0", c1, total)
	}
}
