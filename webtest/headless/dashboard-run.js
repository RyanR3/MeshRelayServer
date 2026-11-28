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
    ok(window.__errors.length === 0, 'no script errors: ' + window.__errors.join(' | '));
  }
  run().catch(e => { ok(false, 'harness threw: ' + e.message); }).finally(() => {
    const pre = document.createElement('pre');
    pre.textContent = '===RESULT===\n' + out.join('\n') + '\n' + (fail ? fail + ' FAILED' : 'ALL PASSED') + '\n===END===';
    document.body.appendChild(pre);
  });
})();
