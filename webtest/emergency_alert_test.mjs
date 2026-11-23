// Tests for the dispatch console's emergency alert state (web/dispatch.js,
// the block between "BEGIN emergency-alert-logic" and "END emergency-alert-logic").
// Run: node --test webtest/emergency_alert_test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const src = readFileSync(fileURLToPath(new URL('../web/dispatch.js', import.meta.url)), 'utf8');
const begin = src.indexOf('// BEGIN emergency-alert-logic');
const end = src.indexOf('// END emergency-alert-logic');
assert.ok(begin >= 0 && end > begin, 'logic markers missing from dispatch.js');
const L = new Function(src.slice(begin, end) +
    '\nreturn { ALARM_REPEAT_MS, mergeEmergency, emergencyIsOpen, openEmergencies, alarmShouldRun, alarmDue, emergencyKind, emergencyHasPosition };')();

// A tiny console: the emergencies map, fed the way handleEmergency feeds it.
function feed(map, m, now) {
    const key = `${m.agency}|${m.user}`;
    const r = L.mergeEmergency(map.get(key), m, now);
    map.set(key, r.e);
    return r;
}
const ack = (map, key) => { const e = map.get(key); if (e && e.active) e.acked = true; };

test('a new emergency is open and starts the alarm', () => {
    const map = new Map();
    const r = feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: true, at: 1000 }, 5000);
    assert.equal(r.raised, true);
    assert.equal(r.changed, true);
    assert.equal(r.e.at, 1000);
    assert.equal(r.e.acked, false);
    assert.equal(L.alarmShouldRun(map.values()), true);
    assert.deepEqual(L.openEmergencies(map.values()).map(e => e.user), ['Engine1']);
});

test('acknowledging stops the alarm and closes the card', () => {
    const map = new Map();
    feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: true, at: 1000 }, 1000);
    ack(map, 'ag|Engine1');
    assert.equal(L.alarmShouldRun(map.values()), false);
    assert.equal(L.openEmergencies(map.values()).length, 0);
});

test('a re-send of an active emergency keeps the acknowledgement and raise time', () => {
    const map = new Map();
    feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: true, at: 1000 }, 1000);
    ack(map, 'ag|Engine1');
    // reconnect re-send (same at) and a TAK-style repeat (new at)
    let r = feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: true, at: 1000 }, 9000);
    assert.equal(r.raised, false); assert.equal(r.changed, false);
    r = feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: true, at: 7000, lat: 41.6, lon: -74.7 }, 9000);
    assert.equal(r.raised, false);
    assert.equal(r.e.at, 1000);
    assert.equal(r.e.acked, true);
    assert.equal(r.e.lat, 41.6);
    assert.equal(L.alarmShouldRun(map.values()), false);
});

test('cleared by the unit without acknowledgement closes it and stops the alarm', () => {
    const map = new Map();
    feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: true, at: 1000 }, 1000);
    const r = feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: false, at: 3000 }, 3000);
    assert.equal(r.changed, true); assert.equal(r.raised, false);
    assert.equal(L.alarmShouldRun(map.values()), false);
    assert.equal(L.openEmergencies(map.values()).length, 0);
});

test('a later emergency from the same unit alerts again after an acknowledged one', () => {
    const map = new Map();
    feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: true, at: 1000 }, 1000);
    ack(map, 'ag|Engine1');
    feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: false, at: 2000 }, 2000);
    const r = feed(map, { agency: 'ag', user: 'Engine1', channel: 3, active: true, at: 5000 }, 5000);
    assert.equal(r.raised, true);
    assert.equal(r.e.acked, false);
    assert.equal(r.e.at, 5000);
    assert.equal(L.alarmShouldRun(map.values()), true);
});

test('two emergencies are handled independently', () => {
    const map = new Map();
    feed(map, { agency: 'ag', user: 'B', channel: 1, active: true, at: 2000 }, 2000);
    feed(map, { agency: 'ag', user: 'A (TAK)', channel: -1, active: true, tak: true, alert: '911 Alert', at: 1000 }, 2000);
    assert.deepEqual(L.openEmergencies(map.values()).map(e => e.user), ['A (TAK)', 'B']); // oldest first
    ack(map, 'ag|B');
    assert.deepEqual(L.openEmergencies(map.values()).map(e => e.user), ['A (TAK)']);
    assert.equal(L.alarmShouldRun(map.values()), true);
    feed(map, { agency: 'ag', user: 'A (TAK)', channel: -1, active: false, tak: true, at: 3000 }, 3000);
    assert.equal(L.alarmShouldRun(map.values()), false);
});

test('page reload (empty console) alerts again for an emergency still active', () => {
    const map = new Map();
    const r = feed(map, { agency: 'ag', user: 'Engine1', channel: 2, active: true, at: 1000 }, 60000);
    assert.equal(r.raised, true);
    assert.equal(r.e.at, 1000);
    assert.equal(L.alarmShouldRun(map.values()), true);
});

test('alarm timing: plays at once, repeats every ALARM_REPEAT_MS, waits for audio', () => {
    const R = L.ALARM_REPEAT_MS;
    assert.ok(R >= 3000 && R <= 6000);
    assert.equal(L.alarmDue(true, true, null, 0), true);
    assert.equal(L.alarmDue(true, true, 0, R - 1), false);
    assert.equal(L.alarmDue(true, true, 0, R), true);
    assert.equal(L.alarmDue(true, false, null, 0), false);   // audio blocked: wait
    assert.equal(L.alarmDue(false, true, null, 0), false);   // nothing open
});

test('kind and position', () => {
    assert.equal(L.emergencyKind({ tak: false }), 'MeshRelay emergency');
    assert.equal(L.emergencyKind({ tak: true, alert: 'Ring The Bell' }), 'Ring The Bell');
    assert.equal(L.emergencyKind({ tak: true }), 'TAK alert');
    assert.equal(L.emergencyHasPosition({}, false), false);
    assert.equal(L.emergencyHasPosition({}, true), true);
    assert.equal(L.emergencyHasPosition({ lat: 0, lon: 0 }, false), true);
});
