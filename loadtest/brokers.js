// Нагрузка на POST /messages: каждое сообщение уходит в PostgreSQL и
// публикуется во все подключенные брокеры. Сценарий держит постоянный
// поток запросов, пока скрипт отказов гасит и возвращает узлы.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const created = new Counter('messages_created');
const failed = new Counter('messages_failed');

const BASE_URL = __ENV.BASE_URL || 'http://app:8080';
const TAG = __ENV.TAG || 'нагрузка';

export const options = {
  scenarios: {
    load: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 100),
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: Number(__ENV.VUS || 50),
      maxVUs: Number(__ENV.MAX_VUS || 300),
    },
  },
  thresholds: {
    // Порог мягкий: во время отказа узла часть запросов может не пройти,
    // тест не должен падать из-за этого, но факт фиксируется.
    http_req_failed: [{ threshold: 'rate<0.5', abortOnFail: false }],
  },
};

export default function () {
  const payload = JSON.stringify({ text: `${TAG} ${__VU}-${__ITER}` });
  const res = http.post(`${BASE_URL}/messages`, payload, {
    headers: { 'Content-Type': 'application/json' },
    timeout: '10s',
  });
  const ok = check(res, { 'сообщение создано': (r) => r.status === 201 });
  if (ok) {
    created.add(1);
  } else {
    failed.add(1);
  }
}
