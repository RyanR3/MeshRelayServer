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
    ok(window.__errors.length === 0, 'no script errors: ' + window.__errors.join(' | '));
  }
  run().catch(e => { ok(false, 'harness threw: ' + e.message); }).finally(() => {
    const pre = document.createElement('pre');
    pre.textContent = '===RESULT===\n' + out.join('\n') + '\n' + (fail ? fail + ' FAILED' : 'ALL PASSED') + '\n===END===';
    document.body.appendChild(pre);
  });
})();
