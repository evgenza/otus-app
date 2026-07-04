import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'https://zhemchugovei.duckdns.org';
const TOKEN_URL = __ENV.TOKEN_URL || `${BASE_URL}/auth/realms/otus/protocol/openid-connect/token`;

export const options = {
  scenarios: {
    single: {
      executor: 'ramping-vus',
      exec: 'single',
      startVUs: 0,
      stages: [
        { duration: '30s', target: 50 },
        { duration: '1m', target: 50 },
        { duration: '15s', target: 0 },
      ],
    },
    e2e: {
      executor: 'ramping-vus',
      exec: 'e2e',
      startVUs: 0,
      stages: [
        { duration: '30s', target: 20 },
        { duration: '1m', target: 20 },
        { duration: '15s', target: 0 },
      ],
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    'http_req_duration{scenario:single}': ['p(95)<200'],
    'http_req_duration{scenario:e2e}': ['p(95)<500'],
  },
};

export function setup() {
  const res = http.post(TOKEN_URL, {
    grant_type: 'password',
    client_id: 'otus-app',
    username: __ENV.AUTH_USER,
    password: __ENV.AUTH_PASS,
  });
  check(res, { 'setup: токен получен': (r) => r.status === 200 });
  return { token: res.json('access_token') };
}

export function single() {
  const res = http.get(`${BASE_URL}/hello?name=k6`);
  check(res, {
    'single: статус 200': (r) => r.status === 200,
    'single: есть сообщение': (r) => r.json('message') !== undefined,
  });
  sleep(0.1);
}

export function e2e(data) {
  const payload = JSON.stringify({ text: `нагрузка ${__VU}-${__ITER}` });
  const headers = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${data.token}`,
  };

  const created = http.post(`${BASE_URL}/messages`, payload, { headers });
  check(created, {
    'e2e: создание 201': (r) => r.status === 201,
    'e2e: вернулся id': (r) => r.json('id') > 0,
  });

  const list = http.get(`${BASE_URL}/messages`);
  check(list, {
    'e2e: список 200': (r) => r.status === 200,
    'e2e: список не пуст': (r) => Array.isArray(r.json()) && r.json().length > 0,
  });
  sleep(0.2);
}
