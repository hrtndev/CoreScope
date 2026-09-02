(function () {
  'use strict';

  var listBody = document.getElementById('regions-list');
  var errEl = document.getElementById('regions-error');
  var saveBtn = document.getElementById('save-regions-btn');
  var saveStatus = document.getElementById('save-status');
  var addBtn = document.getElementById('add-region-btn');
  var newCodeInput = document.getElementById('new-region-code');
  var newNameInput = document.getElementById('new-region-name');

  function getCookie(name) {
    var match = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'));
    return match ? decodeURIComponent(match[1]) : '';
  }

  // Double-submit CSRF — same pattern as admin.js/infrastructure.js.
  function csrfHeaders() {
    return { 'X-CSRF-Token': getCookie('corescope_admin_csrf') };
  }

  function fetchJSON(url, opts) {
    opts = opts || {};
    var headers = Object.assign({}, opts.headers, csrfHeaders());
    return fetch(url, Object.assign({ credentials: 'same-origin' }, opts, { headers: headers })).then(function (res) {
      if (res.status === 401) {
        window.location.href = '/admin/login';
        return Promise.reject(new Error('not logged in'));
      }
      return res.json().then(function (body) {
        if (!res.ok) throw new Error(body.error || ('request failed (' + res.status + ')'));
        return body;
      });
    });
  }

  function regionRow(code, name) {
    var tr = document.createElement('tr');
    tr.dataset.code = code;

    var codeTd = document.createElement('td');
    codeTd.className = 'mono';
    codeTd.textContent = code;
    tr.appendChild(codeTd);

    var nameTd = document.createElement('td');
    var nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.maxLength = 100;
    nameInput.value = name || '';
    nameInput.placeholder = code + ' (no name set — shown as raw code)';
    nameInput.className = 'region-name-input';
    nameTd.appendChild(nameInput);
    tr.appendChild(nameTd);

    var actionTd = document.createElement('td');
    var removeBtn = document.createElement('button');
    removeBtn.type = 'button';
    removeBtn.className = 'toggle on';
    removeBtn.textContent = 'Remove';
    removeBtn.addEventListener('click', function () {
      tr.remove();
    });
    actionTd.appendChild(removeBtn);
    tr.appendChild(actionTd);

    return tr;
  }

  function renderRows(regions, observedCodes) {
    listBody.innerHTML = '';
    var codes = Object.keys(regions);
    observedCodes.forEach(function (c) {
      if (codes.indexOf(c) === -1) codes.push(c);
    });
    codes.sort();
    if (codes.length === 0) {
      var tr = document.createElement('tr');
      tr.className = 'empty-row';
      var td = document.createElement('td');
      td.colSpan = 3;
      td.textContent = 'No IATA regions configured or observed yet.';
      tr.appendChild(td);
      listBody.appendChild(tr);
      return;
    }
    codes.forEach(function (code) {
      listBody.appendChild(regionRow(code, regions[code]));
    });
  }

  function loadRegions() {
    return fetchJSON('/api/admin/regions').then(function (body) {
      renderRows(body.regions || {}, body.observedCodes || []);
    });
  }

  function collectRegionsFromTable() {
    var out = {};
    Array.prototype.forEach.call(listBody.querySelectorAll('tr[data-code]'), function (tr) {
      var code = tr.dataset.code.trim();
      var input = tr.querySelector('.region-name-input');
      var name = input ? input.value.trim() : '';
      if (code && name) out[code] = name;
    });
    return out;
  }

  addBtn.addEventListener('click', function () {
    errEl.textContent = '';
    var code = newCodeInput.value.trim();
    var name = newNameInput.value.trim();
    if (!code || !name) {
      errEl.textContent = 'Enter both a code and a display name.';
      return;
    }
    var existing = listBody.querySelector('tr[data-code="' + code.replace(/"/g, '\\"') + '"]');
    if (existing) {
      existing.querySelector('.region-name-input').value = name;
    } else {
      var emptyRow = listBody.querySelector('.empty-row');
      if (emptyRow) emptyRow.remove();
      listBody.appendChild(regionRow(code, name));
    }
    newCodeInput.value = '';
    newNameInput.value = '';
    newCodeInput.focus();
  });

  saveBtn.addEventListener('click', function () {
    errEl.textContent = '';
    saveStatus.textContent = 'Saving…';
    saveBtn.disabled = true;
    fetchJSON('/api/admin/regions', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ regions: collectRegionsFromTable() })
    })
      .then(function () {
        saveStatus.textContent = 'Saved.';
        return loadRegions();
      })
      .catch(function (err) {
        errEl.textContent = err.message || 'Failed to save';
        saveStatus.textContent = '';
      })
      .then(function () {
        saveBtn.disabled = false;
      });
  });

  fetchJSON('/api/admin/me')
    .then(function (me) {
      window.renderAccountMenu(me);
      return loadRegions();
    })
    .then(function () {
      document.body.classList.add('authed');
    })
    .catch(function (err) {
      if (err.message !== 'not logged in') {
        console.error('[admin-regions]', err);
        document.body.classList.add('authed');
      }
    });
})();
