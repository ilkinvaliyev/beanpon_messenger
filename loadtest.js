// ============================================================================
// beanpon_messenger — load test (k6)
// ----------------------------------------------------------------------------
// Optimizasiya etdiyimiz READ endpoint-lərini yükləyir (yalnız GET — heç bir
// məlumat yaratmır/silmir, prod-da zibil qoymur):
//   GET /api/v1/unread-count            (GetUnreadCount)
//   GET /api/v1/conversations           (GetConversations CTE)
//   GET /api/v1/messages/:other_id      (GetMessages)
//   GET /api/v1/conversation-requests   (GetPendingRequests N+1)
//
// İŞLƏTMƏ:
//   k6 run \
//     -e BASE_URL=https://msg.beanpon.com \
//     -e TOKEN="<JWT>" \
//     -e OTHER_USER_ID=456 \
//     loadtest.js
//
// Yük səviyyəsini dəyişmək: -e PEAK=100 (default 30 paralel istifadəçi).
// ============================================================================

import http from 'k6/http';
import { check, group, sleep } from 'k6';
import { Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const TOKEN = __ENV.TOKEN || '';
const OTHER_USER_ID = __ENV.OTHER_USER_ID || '1';
const PEAK = parseInt(__ENV.PEAK || '30', 10); // eyni anda neçə virtual istifadəçi

// Hər endpoint üçün ayrı latency metrikası (nəticədə ayrı görünsün)
const tUnread = new Trend('ep_unread_count', true);
const tConvos = new Trend('ep_conversations', true);
const tMsgs = new Trend('ep_messages', true);
const tReqs = new Trend('ep_conversation_requests', true);

export const options = {
  // Pilləli artım: yavaş qalx → zirvədə qal → en. Serverin yük altında
  // necə davrandığını və CPU-nun harada doyduğunu göstərir.
  stages: [
    { duration: '30s', target: Math.ceil(PEAK / 3) }, // qızışma
    { duration: '1m', target: PEAK },                  // zirvəyə çıx
    { duration: '2m', target: PEAK },                  // zirvədə sabit qal
    { duration: '30s', target: 0 },                    // soyu
  ],
  thresholds: {
    // Uğur meyarı: istəklərin 95%-i 300ms-dən tez, xəta <1%.
    http_req_duration: ['p(95)<300', 'p(99)<800'],
    http_req_failed: ['rate<0.01'],
  },
};

const params = {
  headers: {
    Authorization: `Bearer ${TOKEN}`,
    'Content-Type': 'application/json',
  },
  // Hər istəyi endpoint adı ilə etiketlə (metrikaları qarışdırma)
  tags: {},
};

function hit(path, trend, name) {
  const res = http.get(`${BASE_URL}/api/v1${path}`, {
    ...params,
    tags: { name },
  });
  trend.add(res.timings.duration);
  check(res, {
    [`${name}: status 200`]: (r) => r.status === 200,
  });
  return res;
}

export default function () {
  // Real istifadəçi davranışını təqlid et: söhbət siyahısı + badge sayı ən
  // tez-tez çağırılır (polling), mesaj açılışı bir az seyrək.
  group('unread-count (polling)', () => {
    hit('/unread-count', tUnread, 'unread_count');
  });

  group('conversations list', () => {
    hit('/conversations', tConvos, 'conversations');
  });

  group('messages with one user', () => {
    hit(`/messages/${OTHER_USER_ID}?page=1&limit=50&peek=true`, tMsgs, 'messages');
  });

  group('pending requests', () => {
    hit('/conversation-requests', tReqs, 'conversation_requests');
  });

  // İstifadəçilər arası düşünmə fasiləsi (yoxsa qeyri-real "tufan" olur)
  sleep(1);
}
