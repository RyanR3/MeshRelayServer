(function () {
  const out = []; let fail = 0;
  const ok = (cond, name) => { out.push((cond ? 'PASS ' : 'FAIL ') + name); if (!cond) fail++; };
  let plays = 0, fakeAudio = false;
  window.alarm = () => { plays++; };           // count patterns instead of sounding them
  window.audioRunning = () => fakeAudio;
  const $ = (id) => document.getElementById(id);
  const shown = () => $('emerg-banner').style.display !== 'none';
  const cards = () => [...document.querySelectorAll('#emerg-banner .eb-card')];
  const card = (user) => cards().find(c => c.dataset.key === 'ag|' + user);
  const wait = (ms) => new Promise(r => setTimeout(r, ms));
  const reload = location.hash === '#reload';
  async function run() {
    await wait(500);
    if (reload) {
      ok(shown() && cards().length === 2, 'reload: both active emergencies re-shown from the server re-send');
      fakeAudio = true; await wait(400);
      ok(plays >= 1, 'reload: alarm runs once audio is allowed');
      return;
    }
    ok(!shown(), 'no banner with no emergency');
    const T = Date.now();
    __emit({ type: 'emergency', agency: 'ag', channel: 2, user: 'Medic5', active: true, at: T, lat: 41.7, lon: -74.8 });
    await wait(300);
    ok(shown() && cards().length === 1, 'banner appears for a new emergency');
    const c = card('Medic5');
    ok(c && /Medic5/.test(c.textContent) && /Test County/.test(c.textContent) && /MeshRelay emergency/.test(c.textContent) && /Fire Dispatch/.test(c.textContent), 'card shows unit, agency, kind, channel: ' + (c && c.textContent.replace(/\s+/g, ' ').trim()));
    ok(plays === 0 && $('emerg-banner-audio').style.display !== 'none', 'audio blocked: no sound yet, blocked note shown');
    fakeAudio = true; await wait(400);
    ok(alarmTimer !== null && plays === 1 && $('emerg-banner-audio').style.display === 'none', 'alarm plays as soon as audio is allowed (no new event)');
    await wait(4000);
    ok(plays === 2, 'alarm repeats after ALARM_REPEAT_MS (plays=' + plays + ')');
    const loc = c.querySelector('.eb-locate');
    ok(!loc.disabled && loc.textContent === 'Locate unit', 'Locate unit enabled when the unit has a position');
    loc.click();
    const pt = map.latLngToContainerPoint([41.7, -74.8]), mr = map.getContainer().getBoundingClientRect(), br = $('emerg-banner').getBoundingClientRect();
    ok(Math.abs(pt.x - mr.width / 2) < 2 && mr.top + pt.y > br.bottom && pt.y < mr.height, 'Locate unit centres the map on the unit, below the banner (y=' + Math.round(mr.top + pt.y) + ' bannerBottom=' + Math.round(br.bottom) + ')');
    __emit({ type: 'emergency', agency: 'ag', channel: -1, user: 'Bravo (TAK)', active: true, tak: true, alert: '911 Alert', at: T + 1000 });
    await wait(100);
    ok(cards().length === 2, 'second emergency gets its own card');
    const b = card('Bravo (TAK)');
    ok(b && /911 Alert/.test(b.textContent) && b.querySelector('.eb-locate').disabled && b.querySelector('.eb-locate').textContent === 'No position', 'TAK card: alert name, "No position" disabled');
    document.body.click(); $('emerg-banner').click();
    await wait(50);
    ok(cards().length === 2, 'clicking elsewhere does not dismiss');
    card('Medic5').querySelector('.eb-ack').click();
    await wait(50);
    ok(cards().length === 1 && card('Bravo (TAK)'), 'acknowledge closes only that card');
    const p0 = plays; await wait(4100);
    ok(plays > p0, 'alarm continues for the remaining unacknowledged emergency');
    // websocket drop + reconnect: server re-sends both active emergencies
    const n0 = window.__wsCount;
    __server.sockets[__server.sockets.length - 1].drop();
    await wait(3500);
    ok(window.__wsCount === n0 + 1, 'websocket reconnected');
    ok(cards().length === 1 && card('Bravo (TAK)') && !card('Medic5'), 'after reconnect: unacked still shown, acked stays acknowledged');
    ok(alarmTimer !== null, 'alarm still scheduled after reconnect');
    // TAK repeat of the same alert does not reset anything
    __emit({ type: 'emergency', agency: 'ag', channel: -1, user: 'Bravo (TAK)', active: true, tak: true, alert: '911 Alert', at: T + 9000 });
    await wait(50);
    ok(cards().length === 1, 'repeat of an active alert does not add a card');
    // cleared by the unit, never acknowledged
    __emit({ type: 'emergency', agency: 'ag', channel: -1, user: 'Bravo (TAK)', active: false, tak: true, at: T + 10000 });
    await wait(50);
    ok(!shown(), 'unit clear closes its card and the banner');
    const p1 = plays; await wait(5000);
    ok(alarmTimer === null && plays === p1, 'alarm stopped after the last open emergency cleared');
    const btn = $('ptt-btn').getBoundingClientRect();
    const hit = document.elementFromPoint(btn.left + btn.width / 2, btn.top + btn.height / 2);
    ok(hit && hit.closest('#ptt-btn'), 'PTT button is clickable after the banner closes (hit=' + (hit && (hit.id || hit.className)) + ' rect=' + JSON.stringify(btn) + ')');
    // new emergency later starts it again
    __emit({ type: 'emergency', agency: 'ag', channel: 2, user: 'Engine1', active: true, at: Date.now() });
    await wait(400);
    ok(shown() && card('Engine1') && plays === p1 + 1, 'a later emergency shows again and restarts the alarm');
    ok(card('Engine1').querySelector('.eb-locate').disabled, 'unit with no live marker and no lat/lon: No position');
    // Medic5 acked earlier, clears and re-raises -> alerts again
    __emit({ type: 'emergency', agency: 'ag', channel: 2, user: 'Medic5', active: false });
    __emit({ type: 'emergency', agency: 'ag', channel: 2, user: 'Medic5', active: true, lat: 41.7, lon: -74.8 });
    await wait(100);
    ok(cards().length === 2, 'a re-raised emergency is unacknowledged again');
    ok(!/[\u{1F300}-\u{1FAFF}☀-➿]/u.test($('emerg-banner').textContent), 'no emoji in banner');
    // Messages to TAK users: one delivered, one not.
    __emit({ type: 'message', id: 'm1', at: Date.now(), agency: 'ag', dir: 'out', from: 'desk', to: 'Echo-1 (TAK)', text: 'sent one', sent: 1, tak_uid: 'ANDROID-1' });
    __emit({ type: 'message', id: 'm2', at: Date.now(), agency: 'ag', dir: 'out', from: 'desk', to: 'Echo-2 (TAK)', text: 'unsent one', tak_uid: 'ANDROID-2', tak_source: 'master', not_sent: 'master TAK not connected' });
    await wait(100);
    const msgs = [...document.querySelectorAll('#msg-list .msg')];
    const m1 = msgs.find(m => /sent one/.test(m.textContent) && !/unsent/.test(m.textContent)), m2 = msgs.find(m => /unsent one/.test(m.textContent));
    ok(m1 && !/not delivered/.test(m1.textContent), 'delivered TAK message not flagged');
    ok(m2 && /not delivered \(master TAK not connected\)/.test(m2.textContent), 'undelivered TAK message flagged with the reason: ' + (m2 && m2.textContent));
    // The console sends the callsign it showed along with the uid.
    const sock = __server.sockets[__server.sockets.length - 1], sentMsgs = [];
    sock.send = (d) => sentMsgs.push(JSON.parse(d));
    messageTak('ag', 'ANDROID-9', 'Echo-9', 'master');
    $('msg-text').value = 'hello'; sendMessage();
    const sm = sentMsgs.find(m => m.type === 'message');
    ok(sm && sm.tak && sm.tak.uid === 'ANDROID-9' && sm.tak.source === 'master' && sm.tak.callsign === 'Echo-9', 'TAK message carries the shown callsign: ' + JSON.stringify(sm));
  }
  run().catch(e => { out.push('FAIL exception ' + e.stack); fail++; }).finally(() => {
    const pre = document.createElement('pre'); pre.id = 'harness-result';
    pre.textContent = '\n===RESULT===\n' + out.join('\n') + '\n' + (fail ? fail + ' FAILED' : 'ALL PASSED') + '\n===END===\n';
    document.body.appendChild(pre);
  });
})();
