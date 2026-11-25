// Measures the latency the LLM Gateway adds. The same streamed chat request is sent
// directly to a fake provider that answers instantly, then through the gateway to that same
// provider. The difference is the gateway's own cost: auth, workspace policy (Postgres),
// rate limit (Redis), re-encoding the stream, and writing the usage record (Postgres).
//
// Run it with `make bench-gateway`, which starts the fake provider and the gateway.
import http from 'k6/http';
import { check } from 'k6';

const vus = 10;
const duration = 20;

export const options = {
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    direct: { executor: 'constant-vus', vus, duration: `${duration}s`, exec: 'direct' },
    gateway: { executor: 'constant-vus', vus, duration: `${duration}s`, exec: 'viaGateway', startTime: `${duration + 2}s` },
  },
  // Thresholds on per-scenario sub-metrics make k6 report each scenario separately. They are
  // deliberately loose: this script measures, it does not gate.
  thresholds: {
    'http_req_duration{scenario:direct}': ['p(95)<5000'],
    'http_req_duration{scenario:gateway}': ['p(95)<5000'],
    'http_req_waiting{scenario:direct}': ['p(95)<5000'],
    'http_req_waiting{scenario:gateway}': ['p(95)<5000'],
    'checks': ['rate>0.99'],
  },
};

const body = JSON.stringify({
  model: 'bench',
  stream: true,
  messages: [{ role: 'user', content: 'benchmark' }],
});

export function direct() {
  const res = http.post(`${__ENV.PROVIDER_URL}/v1/chat/completions`, body.replace('"bench"', '"fake-model"'), {
    headers: { 'Content-Type': 'application/json' },
  });
  check(res, { 'direct 200': (r) => r.status === 200 && r.body.includes('[DONE]') });
}

export function viaGateway() {
  const res = http.post(`${__ENV.GATEWAY_URL}/v1/chat/completions`, body, {
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${__ENV.GATEWAY_TOKEN}`,
      'X-Enclave-Workspace-ID': __ENV.WORKSPACE_ID,
    },
  });
  check(res, { 'gateway 200': (r) => r.status === 200 && r.body.includes('[DONE]') });
}

function row(name, metric) {
  const v = metric.values;
  const ms = (x) => x.toFixed(2).padStart(8);
  return `${name.padEnd(34)}${ms(v.avg)}${ms(v['p(50)'])}${ms(v['p(95)'])}${ms(v['p(99)'])}`;
}

// Prints a compact comparison table instead of k6's long default summary.
export function handleSummary(data) {
  const m = data.metrics;
  const direct = m['http_req_duration{scenario:direct}'];
  const gateway = m['http_req_duration{scenario:gateway}'];
  const lines = [
    '',
    `${'(milliseconds)'.padEnd(34)}${'avg'.padStart(8)}${'p50'.padStart(8)}${'p95'.padStart(8)}${'p99'.padStart(8)}`,
    row('full response, direct', direct),
    row('full response, via gateway', gateway),
    row('first byte, direct', m['http_req_waiting{scenario:direct}']),
    row('first byte, via gateway', m['http_req_waiting{scenario:gateway}']),
    '',
    `requests: ${m.http_reqs.values.count}, checks passed: ${(m.checks.values.rate * 100).toFixed(2)}%`,
    `gateway overhead at p50: ${(gateway.values['p(50)'] - direct.values['p(50)']).toFixed(2)} ms, at p95: ${(gateway.values['p(95)'] - direct.values['p(95)']).toFixed(2)} ms`,
    '',
  ];
  return { stdout: lines.join('\n') };
}
