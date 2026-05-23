<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * message_moderation_logs
 * ------------------------------------------------------------------
 * Bu tablo, messenger (Go) servisi tərəfindən doldurulur. Hər mesaj
 * şifrələnmədən ƏVVƏL arxa planda (queue worker) AI ilə analiz edilir.
 * Yalnız AI bir RİSK kateqoriyası aşkar etdikdə bura bir sətir yazılır
 * (təmiz mesajlar üçün heç nə yazılmır — boş yer tutmamaq üçün).
 *
 * Tablo yalnız moderasiya/audit məqsədlidir. Mesajın özü burada AÇIQ
 * mətn kimi saxlanmır; yalnız qısa snippet (sübut üçün) saxlanılır.
 *
 * Filament/admin panel bu tablonu oxuyub şübhəli istifadəçiləri
 * izləyə bilər.
 */
return new class extends Migration {
    public function up(): void
    {
        Schema::create('message_moderation_logs', function (Blueprint $table) {
            $table->bigIncrements('id');

            // Messenger DB-də mesajların id-si UUID-dir (messages.id uuid).
            // Burada string saxlayırıq; FK qoymuruq çünki messages tablosu
            // həm də messenger servisinin idarəsindədir və mesaj sonradan
            // soft-delete oluna bilər — log isə qalmalıdır.
            $table->uuid('message_id')->nullable()->index();

            // Kim kimə göndərdi
            $table->unsignedBigInteger('sender_id')->index();
            $table->unsignedBigInteger('receiver_id')->nullable()->index();

            // AI-nın təyin etdiyi kateqoriya. Mümkün dəyərlər:
            //   threat               — birisi digərini hədələyir
            //   illegal_goods        — narkotik/silah və s. qanunsuz alqı-satqı
            //   anti_state           — dövlət əleyhinə fəaliyyət/təşviq
            //   off_platform         — söhbəti tətbiqdən kənara çıxarma cəhdi
            //                          (Instagram, TikTok, Telegram, WhatsApp...)
            //   harassment           — təkrarlanan təcavüz/aşağılama
            //   scam                 — dələduzluq / fırıldaq
            //   csae                 — uşaq təhlükəsizliyi (ən yüksək prioritet)
            //   self_harm            — özünə zərər / intihar riski
            $table->string('category', 40)->index();

            // AI-nın əminlik dərəcəsi (0.00 - 1.00)
            $table->decimal('confidence', 4, 2)->default(0);

            // Risk səviyyəsi — kateqoriyaya görə avtomatik təyin olunur:
            //   low / medium / high / critical
            $table->string('severity', 16)->default('medium')->index();

            // AI-nın qısa izahı (niyə bu kateqoriya seçildi)
            $table->text('reason')->nullable();

            // Mesajın qısa parçası (sübut üçün, maksimum 500 simvol).
            // Tam mesaj burada saxlanmır.
            $table->string('snippet', 500)->nullable();

            // Arxa planda bu loga görə hansı aksiyalar görüldü
            // (JSON массив: ["notify_sender","admin_alert"] kimi).
            $table->json('actions_taken')->nullable();

            // Göndərənə xəbərdarlıq notification-ı getdimi?
            // (yalnız off_platform kateqoriyasında true olur)
            $table->boolean('sender_notified')->default(false);

            // Admin tərəfindən baxıldı / həll edildi?
            $table->boolean('is_reviewed')->default(false)->index();
            $table->unsignedBigInteger('reviewed_by')->nullable();
            $table->timestamp('reviewed_at')->nullable();

            // AI cavabının xam JSON-u (debug / audit üçün)
            $table->json('ai_raw_response')->nullable();

            $table->timestamps();

            // Tez-tez işlənəcək sorğular üçün kompozit indekslər
            $table->index(['sender_id', 'category']);
            $table->index(['category', 'created_at']);
            $table->index(['is_reviewed', 'severity']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('message_moderation_logs');
    }
};
