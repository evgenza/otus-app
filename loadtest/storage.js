// Нагрузка на распределенные хранилища: сообщения уходят в PostgreSQL и
// дальше через Kafka в Cassandra и Elasticsearch, файлы кладутся в S3 и
// читаются диапазонами. Сценарий держит поток запросов, пока скрипт
// отказов гасит узлы MinIO, HDFS, Cassandra и Elasticsearch.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const okCounter = new Counter('ops_ok');
const failCounter = new Counter('ops_failed');

const BASE_URL = __ENV.BASE_URL || 'http://app:8080';
const OBJECT_SIZE = Number(__ENV.OBJECT_SIZE || 4096);
const READ_KEY = 'loadtest-sample.bin';

export const options = {
  scenarios: {
    messages: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE_MESSAGES || 60),
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: 30, maxVUs: 200, exec: 'postMessage',
    },
    uploads: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE_UPLOAD || 20),
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: 20, maxVUs: 100, exec: 'uploadObject',
    },
    ranges: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE_READ || 20),
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: 20, maxVUs: 100, exec: 'readRange',
    },
  },
  thresholds: {
    http_req_failed: [{ threshold: 'rate<0.9', abortOnFail: false }],
  },
};

function body(size) {
  return 'x'.repeat(size);
}

export function setup() {
  // Эталонный объект, куски которого читает сценарий ranges.
  http.post(`${BASE_URL}/files?name=${READ_KEY}&tags=kind=loadtest`, body(65536), {
    headers: { 'Content-Type': 'application/octet-stream' },
  });
}

export function postMessage() {
  const res = http.post(`${BASE_URL}/messages`,
    JSON.stringify({ text: `нагрузка хранилищ ${__VU}-${__ITER}` }),
    { headers: { 'Content-Type': 'application/json' }, timeout: '15s' });
  count(check(res, { 'сообщение создано': (r) => r.status === 201 }));
}

export function uploadObject() {
  const res = http.post(
    `${BASE_URL}/files?name=obj-${__VU}-${__ITER}.bin&tags=kind=loadtest,vu=${__VU}`,
    body(OBJECT_SIZE),
    { headers: { 'Content-Type': 'application/octet-stream' }, timeout: '15s' });
  count(check(res, { 'объект загружен': (r) => r.status === 201 }));
}

export function readRange() {
  const offset = (__ITER * 1024) % 60000;
  const res = http.get(`${BASE_URL}/files/${READ_KEY}`, {
    headers: { Range: `bytes=${offset}-${offset + 1023}` },
    timeout: '15s',
  });
  count(check(res, { 'кусок получен': (r) => r.status === 206 && r.body.length === 1024 }));
}

function count(ok) {
  if (ok) {
    okCounter.add(1);
  } else {
    failCounter.add(1);
  }
}
