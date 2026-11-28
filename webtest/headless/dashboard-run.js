(function () {
  const out = []; let fail = 0;
  const ok = (cond, name) => { out.push((cond ? 'PASS ' : 'FAIL ') + name); if (!cond) fail++; };
  const $ = (id) => document.getElementById(id);
  const wait = (ms) => new Promise(r => setTimeout(r, ms));
  const status = () => ($('migration-status').textContent || '');
  async function run() {
    await wait(400);
    // Every page opens without a script error.
    document.querySelectorAll('.nav-item[data-page]').forEach(n => { try { navigateTo(n.dataset.page); } catch (e) { window.__errors.push(n.dataset.page + ': ' + e.message); } });
    await wait(300);
    navigateTo('settings');
    await wait(300);
    const opts = [...$('mig-export-agency').options].map(o => o.value);
    ok(opts.join(',') === ',fire,ems', 'export agency list filled: ' + opts.join(','));

    // Export: passphrase checks, then the request.
    $('mig-export-pass').value = 'short'; $('mig-export-pass2').value = 'short';
    migExport(); await wait(50);
    ok(/at least 12/.test(status()), 'short passphrase refused in the page');
    $('mig-export-pass').value = 'a long passphrase 1'; $('mig-export-pass2').value = 'a long passphrase 2';
    migExport(); await wait(50);
    ok(/not the same/.test(status()), 'mismatched passphrases refused');
    $('mig-export-pass2').value = 'a long passphrase 1';
    $('mig-export-agency').value = 'fire';
    $('mig-export-identity').checked = true;
    migExport(); await wait(300);
    ok(window.__exportBody && window.__exportBody.agency === 'fire' && window.__exportBody.include_identity_key === true &&
       window.__exportBody.passphrase === 'a long passphrase 1', 'export request carries agency, key choice, passphrase');
    ok(/Export downloaded/.test(status()) && $('mig-export-pass').value === '', 'export done, passphrase field cleared');

    // Import: preview needed first.
    ok($('mig-import-btn').disabled, 'Import disabled before a preview');
    const dt = new DataTransfer();
    dt.items.add(new File(['{"format":"meshrelay-user-export"}'], 'users.mrusers'));
    $('mig-import-file').files = dt.files;
    $('mig-import-pass').value = 'wrong';
    migPreview(); await wait(300);
    ok(/Preview failed: wrong passphrase/.test(status()) && $('mig-import-btn').disabled, 'wrong passphrase shown, Import stays off');
    $('mig-import-pass').value = 'the real passphrase';
    migPreview(); await wait(300);
    ok(window.__previewForm && window.__previewForm.file === 'FILE:users.mrusers' && window.__previewForm.mode === 'skip' &&
       window.__previewForm.create_agencies === '1' && window.__previewForm.import_web_admins === '', 'preview form fields: ' + JSON.stringify(window.__previewForm));
    const rows = [...document.querySelectorAll('#mig-preview-body tr')];
    ok(rows.length === 4, 'preview table rows: ' + rows.length);
    ok(!window.__xss && !document.querySelector('#mig-preview-body img') && !document.querySelector('#mig-preview-body b'), 'names in the preview are escaped');
    ok(rows[1] && /Medic "One"/.test(rows[1].textContent) && /1 phone key/.test(rows[1].textContent), 'user row text: ' + (rows[1] && rows[1].textContent));
    ok(!$('mig-use-identity').disabled, 'identity option offered when the file has a key');
    ok(!$('mig-import-btn').disabled, 'Import enabled after the preview');
    $('mig-import-mode').value = 'replace'; migInputsChanged();
    ok($('mig-import-btn').disabled, 'changing an option needs a new preview');
    migPreview(); await wait(300);
    ok(!$('mig-import-btn').disabled && window.__previewForm.mode === 'replace', 'new preview with replace');
    migApply(); await wait(300);
    ok(/Import done: 1 agencies, 1 phone accounts/.test(status()), 'import result shown: ' + status());
    ok($('mig-import-btn').disabled && $('mig-import-pass').value === '', 'after import: button off, passphrase cleared');

    // TAK: the agency's share switch (master set up) and the master panel.
    await wait(200);
    ok($('tak-share-switch').style.display !== 'none' && !$('tak-share-master').checked, 'share switch shown, off');
    ok(/Master TAK server: connected/.test($('tak-share-state').textContent), 'master status on the agency page: ' + $('tak-share-state').textContent);
    ok($('tak-share-same').style.display !== 'none', 'same-server note shown');
    ok(/come back to the agency that wrote to them/.test($('tak-share-routing').textContent) && /go to EMS's console/.test($('tak-share-routing').textContent),
       'routing line: ' + $('tak-share-routing').textContent);
    ok(!$('tak-share-master').disabled && $('tak-share-home').style.display === 'none', 'not the home agency: switch free');
    renderTakShare({ share_master: true, master_home: true, master: { configured: true, status: 'connected', home_agency_name: 'Fire' } });
    ok($('tak-share-master').checked && $('tak-share-master').disabled && $('tak-share-home').style.display !== 'none', 'home agency: switch on, locked, reason shown');
    renderTakShare({ share_master: false, master_home: false, master: { configured: true, status: 'connected', home_agency_name: 'EMS' } });
    $('tak-share-master').checked = true; saveTakShareMaster(); await wait(200);
    ok(window.__shareBody && window.__shareBody.share === true && window.__shareBody.agency_id === 'fire', 'share switch PUT: ' + JSON.stringify(window.__shareBody));
    ok($('takm-host').value === 'tak.example.org' && /Connected/.test($('takm-state').textContent), 'master panel filled');
    ok(/Fire <b>Dept<\/b>/.test($('takm-sharing').textContent) && !document.querySelector('#takm-sharing b') &&
       /same server/.test($('takm-sharing').textContent) && /EMS/.test($('takm-sharing').textContent), 'sharing list, escaped: ' + $('takm-sharing').textContent);
    ok(/home agency/.test($('takm-sharing').textContent), 'home agency marked in the sharing list');
    ok($('takm-home-row').style.display !== 'none' && $('takm-home').value === 'fire' && $('takm-home').options.length === 2 &&
       /Fire <b>Dept<\/b>/.test($('takm-home').options[1].textContent) && /not chosen yet/.test($('takm-home-state').textContent), 'home selector: ' + $('takm-home-state').textContent);
    $('takm-home').value = 'ems'; saveTakMasterHome(); await wait(200);
    ok(window.__homeBody && window.__homeBody.agency_id === 'ems' && $('takm-home').value === 'ems' && /In effect: EMS$/.test($('takm-home-state').textContent), 'home PUT: ' + JSON.stringify(window.__homeBody));
    $('takm-password').value = 'pw-not-kept'; saveTakMaster(); await wait(200);
    ok(window.__takMasterBody && window.__takMasterBody.host === 'tak.example.org' && window.__takMasterBody.password === 'pw-not-kept' &&
       $('takm-password').value === '', 'master save PUT, password field cleared');

    // LMR bridges, super admin: switches and toggles re-render the list once,
    // never twice, and an older reload answering late does not win.
    navigateTo('lmr-bridges'); await wait(300);
    const superCards = () => document.querySelectorAll('#lmr-bridges-list .ma-patch-existing').length;
    const superBuilders = () => document.querySelectorAll('#lmr-bridges-list .ma-patch-builder').length;
    ok(superCards() === 2 && superBuilders() === 1, 'LMR super list: ' + superCards() + ' bridges, ' + superBuilders() + ' builder');
    document.querySelector('#lmr-bridges-list .lmr-name').value = 'typed name';
    toggleLmrBridge('b1'); toggleLmrBridge('b1'); await wait(300);
    ok(superCards() === 2 && superBuilders() === 1, 'LMR super list after two quick toggles: ' + superCards() + ' bridges, ' + superBuilders() + ' builder');
    ok(document.querySelector('#lmr-bridges-list .lmr-name').value === 'typed name', 'open builder keeps what was typed');
    const sw1 = () => document.querySelector('#lmr-bridges-list .lmr-atak[data-bridge="b1"] .lmr-atak-on');
    window.__listDelays = [200, 10];          // the first reload answers last
    sw1().checked = false; lmrAtakSave(sw1()); await wait(30);
    sw1().checked = true; lmrAtakSave(sw1()); await wait(400);
    ok(superCards() === 2 && superBuilders() === 1, 'LMR super list after two quick ATAK switches: ' + superCards() + ' bridges');
    ok(sw1().checked === true && window.__lmr[0].show_in_atak === true, 'the newest answer wins (switch on, as saved last)');
    const b1State = document.querySelector('#lmr-bridges-list .lmr-atak[data-bridge="b1"]').textContent;
    ok(/Fire Dept: sent through the master TAK server\./.test(b1State) && /EMS: sent to its own TAK server\. Same server as the master, sent once\./.test(b1State)
       && !/no TAK connection/.test(b1State), 'super admin ATAK state: ' + b1State.replace(/\s+/g, ' '));

    // Mutual aid, super admin: the same list pattern.
    navigateTo('mutual-aid'); await wait(200);
    const maCards = () => document.querySelectorAll('#ma-patches .ma-patch-existing').length;
    fetchMutualAid(); fetchMutualAid(); await wait(200);
    ok(maCards() === 1 && document.querySelectorAll('#ma-patches .ma-patch-builder').length === 1, 'mutual aid list after reloads: ' + maCards());

    // LMR bridges, agency admin view (rendered directly; the harness signs in
    // as super admin): pause and guests, twice quickly each.
    fetchAgencyLmrBridges(); await wait(200);
    const agencyCards = () => document.querySelectorAll('#lmr-agency-list .ma-patch-card').length;
    ok(agencyCards() === 2, 'LMR agency list: ' + agencyCards());
    window.__listDelays = [150, 10];
    lmrToggle('a1', false); await wait(20); lmrToggle('a1', true); await wait(300);
    ok(agencyCards() === 2 && !document.querySelector('#lmr-agency-list .lmr-paused'), 'agency list after pause+resume: ' + agencyCards() + ' cards, not paused');
    lmrGuests('a1', false); lmrGuests('a1', true); await wait(200);
    ok(agencyCards() === 2, 'agency list after two guest switches: ' + agencyCards());
    const a1State = document.querySelector('#lmr-agency-list .lmr-atak[data-bridge="a1"] .lmr-atak-state').textContent;
    ok(a1State === 'Marker is being sent through the master TAK server.', 'agency ATAK state (master only): ' + a1State);

    // The agency admin's ATAK line for every combination of its own TAK link
    // and the master.
    const line = (tak, master, sent) => {
        const d = document.createElement('div');
        d.innerHTML = lmrAtakAgencyState({ show_in_atak: true, enabled: true, atak_tak: tak, atak_master: master, atak_sent: sent });
        return d.textContent;
    };
    const own = 'your agency has no TAK connection set up';
    [
        ['connected', 'not_shared', 'own', 'Marker is being sent to your TAK server.'],
        ['connected', 'none', 'own', 'Marker is being sent to your TAK server.'],
        ['none', 'connected', 'master', 'Marker is being sent through the master TAK server.'],
        ['none', 'connecting', 'none', 'Marker not being sent: ' + own + '; the master TAK server is not connected right now (sent when it connects).'],
        ['none', 'off', 'none', 'Marker not being sent: ' + own + '; the master TAK connection is switched off.'],
        ['none', 'not_shared', 'none', 'Marker not being sent: ' + own + '; your agency does not share with the master TAK server.'],
        ['connected', 'same_server', 'own', 'Marker is being sent to your TAK server. The master TAK server is the same server, so it is sent once.'],
        ['down', 'same_server', 'none', 'Marker not being sent: your TAK connection is down (sent again when it reconnects); the master TAK server is the same server as yours, so the marker goes on your own connection only.'],
        ['connected', 'connected', 'both', 'Marker is being sent to both your TAK server and the master TAK server.'],
        ['down', 'connected', 'master', 'Marker is being sent through the master TAK server, but not to your TAK server: your TAK connection is down (sent again when it reconnects).'],
        ['off', 'connected', 'master', 'Marker is being sent through the master TAK server, but not to your TAK server: your TAK connection is switched off.'],
        ['connected', 'connecting', 'own', 'Marker is being sent to your TAK server, but not through the master TAK server: the master TAK server is not connected right now (sent when it connects).'],
        ['none', 'none', 'none', 'Marker not being sent: ' + own + '.']
    ].forEach(([tak, master, sent, want]) => {
        const got = line(tak, master, sent);
        ok(got === want, 'ATAK line ' + tak + '/' + master + '/' + sent + ': ' + got);
    });
    ok(window.__errors.length === 0, 'no script errors: ' + window.__errors.join(' | '));
  }
  run().catch(e => { ok(false, 'harness threw: ' + e.message); }).finally(() => {
    const pre = document.createElement('pre');
    pre.textContent = '===RESULT===\n' + out.join('\n') + '\n' + (fail ? fail + ' FAILED' : 'ALL PASSED') + '\n===END===';
    document.body.appendChild(pre);
  });
})();
