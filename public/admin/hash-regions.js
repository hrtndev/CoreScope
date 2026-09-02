(function () {
  'use strict';

  var listBody = document.getElementById('regions-list');
  var errEl = document.getElementById('regions-error');
  var saveBtn = document.getElementById('save-regions-btn');
  var saveStatus = document.getElementById('save-status');
  var addBtn = document.getElementById('add-region-btn');
  var newNameInput = document.getElementById('new-region-name');

  function getCookie(name) {
    var match = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'));
    return match ? decodeURIComponent(match[1]) : '';
  }

  // Double-submit CSRF — same pattern as admin.js/infrastructure.js/regions.js.
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

  // Mirrors loadRegionKeys' normalization in cmd/ingestor/main.go so what's
  // shown here matches what the ingestor will actually derive keys from.
  function normalize(name) {
    name = name.trim();
    if (!name) return '';
    return name.charAt(0) === '#' ? name : '#' + name;
  }

  function regionRow(name) {
    var tr = document.createElement('tr');

    var nameTd = document.createElement('td');
    nameTd.className = 'mono';
    nameTd.textContent = name;
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

  function renderRows(regions) {
    listBody.innerHTML = '';
    if (!regions || regions.length === 0) {
      var tr = document.createElement('tr');
      tr.className = 'empty-row';
      var td = document.createElement('td');
      td.colSpan = 2;
      td.textContent = 'No regions configured yet.';
      tr.appendChild(td);
      listBody.appendChild(tr);
      return;
    }
    regions.forEach(function (name) {
      listBody.appendChild(regionRow(name));
    });
  }

  function loadRegions() {
    return fetchJSON('/api/admin/hash-regions').then(function (body) {
      renderRows(body.hashRegions || []);
    });
  }

  function collectRegionsFromTable() {
    var out = [];
    Array.prototype.forEach.call(listBody.querySelectorAll('td.mono'), function (td) {
      out.push(td.textContent);
    });
    return out;
  }

  addBtn.addEventListener('click', function () {
    errEl.textContent = '';
    var name = normalize(newNameInput.value);
    if (!name) {
      errEl.textContent = 'Enter a region name.';
      return;
    }
    var already = collectRegionsFromTable().indexOf(name) !== -1;
    if (!already) {
      var emptyRow = listBody.querySelector('.empty-row');
      if (emptyRow) emptyRow.remove();
      listBody.appendChild(regionRow(name));
    }
    newNameInput.value = '';
    newNameInput.focus();
  });

  newNameInput.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') {
      e.preventDefault();
      addBtn.click();
    }
  });

  saveBtn.addEventListener('click', function () {
    errEl.textContent = '';
    saveStatus.textContent = 'Saving…';
    saveBtn.disabled = true;
    fetchJSON('/api/admin/hash-regions', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ hashRegions: collectRegionsFromTable() })
    })
      .then(function () {
        saveStatus.textContent = 'Saved. Takes effect within ~15s — no restart needed.';
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
        console.error('[admin-hash-regions]', err);
        document.body.classList.add('authed');
      }
    });
})();
