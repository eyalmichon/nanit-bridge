(function() {
  var nanitStatusText = document.getElementById('nanitStatusText');
  var nanitLoginForm = document.getElementById('nanitLoginForm');
  var nanitMfaForm = document.getElementById('nanitMfaForm');
  var nanitLoginBtn = document.getElementById('nanitLoginBtn');
  var nanitMfaBtn = document.getElementById('nanitMfaBtn');
  var nanitEmail = document.getElementById('nanitEmail');
  var nanitPassword = document.getElementById('nanitPassword');
  var nanitMfaCode = document.getElementById('nanitMfaCode');
  var nanitError = document.getElementById('nanitError');
  var nanitSuccess = document.getElementById('nanitSuccess');

  var passwordForm = document.getElementById('passwordForm');
  var passwordBtn = document.getElementById('passwordBtn');
  var currentPassword = document.getElementById('currentPassword');
  var newPassword = document.getElementById('newPassword');
  var confirmPassword = document.getElementById('confirmPassword');
  var passwordError = document.getElementById('passwordError');
  var passwordSuccess = document.getElementById('passwordSuccess');

  function showError(el, msg) {
    el.textContent = msg || 'Request failed.';
    el.classList.remove('hidden');
  }

  function hideError(el) {
    el.textContent = '';
    el.classList.add('hidden');
  }

  function showSuccess(el, msg) {
    el.textContent = msg || 'Saved.';
    el.classList.remove('hidden');
  }

  function hideSuccess(el) {
    el.textContent = '';
    el.classList.add('hidden');
  }

  function handleAuthError(r) {
    if (r && r.status === 401) {
      window.location.href = '/login';
      return true;
    }
    return false;
  }

  function fetchJSON(url, options) {
    return fetch(url, options).then(function(r) {
      if (handleAuthError(r)) throw new Error('unauthorized');
      if (!r.ok) {
        return r.text().then(function(t) {
          throw new Error(t || (r.status + ' ' + r.statusText));
        });
      }
      return r.json();
    });
  }

  function refreshNanitStatus() {
    return fetchJSON('/api/nanit/status').then(function(d) {
      if (d.connected) {
        nanitStatusText.textContent = 'Connected as ' + (d.email || 'unknown account') + '.';
        nanitLoginBtn.textContent = 'Reconnect';
        nanitLoginForm.classList.remove('hidden');
        nanitMfaForm.classList.add('hidden');
      } else if (d.mfa_pending) {
        nanitStatusText.textContent = 'MFA verification required — enter the code from your authenticator app.';
        nanitLoginForm.classList.add('hidden');
        nanitMfaForm.classList.remove('hidden');
      } else {
        nanitStatusText.textContent = 'Not connected to Nanit cloud.';
        nanitLoginBtn.textContent = 'Connect';
        nanitLoginForm.classList.remove('hidden');
        nanitMfaForm.classList.add('hidden');
      }
    }).catch(function(err) {
      if (err.message !== 'unauthorized') {
        nanitStatusText.textContent = 'Failed to load Nanit status.';
      }
    });
  }

  nanitLoginForm.addEventListener('submit', function(e) {
    e.preventDefault();
    hideError(nanitError);
    hideSuccess(nanitSuccess);
    nanitLoginBtn.disabled = true;
    nanitLoginBtn.textContent = 'Connecting...';

    fetchJSON('/api/nanit/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: nanitEmail.value, password: nanitPassword.value })
    }).then(function(d) {
      if (d.status === 'mfa_required') {
        nanitMfaForm.classList.remove('hidden');
        showSuccess(nanitSuccess, 'MFA required. Enter the code from your phone.');
      } else {
        nanitMfaForm.classList.add('hidden');
        showSuccess(nanitSuccess, 'Connected successfully.');
      }
      return refreshNanitStatus();
    }).catch(function(err) {
      showError(nanitError, err.message);
    }).finally(function() {
      nanitLoginBtn.disabled = false;
      nanitLoginBtn.textContent = 'Connect';
    });
  });

  nanitMfaForm.addEventListener('submit', function(e) {
    e.preventDefault();
    hideError(nanitError);
    hideSuccess(nanitSuccess);
    nanitMfaBtn.disabled = true;
    nanitMfaBtn.textContent = 'Verifying...';

    fetchJSON('/api/nanit/mfa', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code: nanitMfaCode.value })
    }).then(function() {
      nanitMfaForm.classList.add('hidden');
      nanitMfaCode.value = '';
      showSuccess(nanitSuccess, 'Connected successfully.');
      return refreshNanitStatus();
    }).catch(function(err) {
      showError(nanitError, err.message);
    }).finally(function() {
      nanitMfaBtn.disabled = false;
      nanitMfaBtn.textContent = 'Verify MFA';
    });
  });

  passwordForm.addEventListener('submit', function(e) {
    e.preventDefault();
    hideError(passwordError);
    hideSuccess(passwordSuccess);

    if (!currentPassword.value || !newPassword.value || !confirmPassword.value) {
      showError(passwordError, 'Please fill in all password fields.');
      return;
    }
    if (newPassword.value !== confirmPassword.value) {
      showError(passwordError, 'New passwords do not match.');
      return;
    }

    passwordBtn.disabled = true;
    passwordBtn.textContent = 'Saving...';

    fetch('/api/auth/change-password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        current_password: currentPassword.value,
        new_password: newPassword.value,
        confirm_password: confirmPassword.value
      })
    }).then(function(r) {
      if (handleAuthError(r)) return;
      if (!r.ok) return r.text().then(function(t) { throw new Error(t || 'Failed to change password'); });
      showSuccess(passwordSuccess, 'Password changed. Please sign in again.');
      setTimeout(function() { window.location.href = '/login'; }, 900);
    }).catch(function(err) {
      showError(passwordError, err.message);
    }).finally(function() {
      passwordBtn.disabled = false;
      passwordBtn.textContent = 'Change password';
    });
  });

  document.getElementById('logoutBtn').onclick = function() {
    fetch('/api/auth/logout', { method: 'POST' }).finally(function() {
      window.location.href = '/login';
    });
  };

  // ── RTMP Token ──────────────────────────────────────────

  var rtmpTokenInput = document.getElementById('rtmpToken');
  var streamUrlList = document.getElementById('streamUrlList');
  var copyTokenBtn = document.getElementById('copyTokenBtn');
  var regenerateBtn = document.getElementById('regenerateTokenBtn');
  var rtmpError = document.getElementById('rtmpError');
  var rtmpSuccess = document.getElementById('rtmpSuccess');

  var currentTokenData = null;
  var currentBabies = [];

  function renderStreamUrls() {
    if (!currentTokenData) return;
    var base = (currentTokenData.url_template || '').replace('{token}', currentTokenData.token || '');
    rtmpTokenInput.value = currentTokenData.token || '';

    if (currentBabies.length === 0) {
      streamUrlList.innerHTML = '<p class="settings-subtle">No babies connected yet.</p>';
      return;
    }

    streamUrlList.innerHTML = '';
    currentBabies.forEach(function(b) {
      var url = base.replace('{uid}', b.uid);
      var row = document.createElement('div');
      row.className = 'stream-url-row';

      var label = document.createElement('span');
      label.className = 'stream-url-name';
      label.textContent = b.name || b.uid;
      row.appendChild(label);

      var urlWrap = document.createElement('div');
      urlWrap.className = 'input-with-btn';

      var input = document.createElement('input');
      input.type = 'text';
      input.readOnly = true;
      input.value = url;
      urlWrap.appendChild(input);

      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'copy-btn';
      btn.textContent = 'Copy';
      btn.addEventListener('click', function() { copyToClipboard(url, btn); });
      urlWrap.appendChild(btn);

      row.appendChild(urlWrap);
      streamUrlList.appendChild(row);
    });
  }

  function loadRTMPToken() {
    fetchJSON('/api/rtmp/token').then(function(d) {
      currentTokenData = d;
      renderStreamUrls();
    }).catch(function(err) {
      if (err.message !== 'unauthorized') {
        showError(rtmpError, 'Failed to load RTMP token.');
      }
    });
  }

  function loadBabies() {
    fetchJSON('/api/babies').then(function(d) {
      currentBabies = (d.babies || []);
      renderStreamUrls();
    }).catch(function() {});
  }

  function copyToClipboard(text, btn) {
    navigator.clipboard.writeText(text).then(function() {
      var orig = btn.textContent;
      btn.textContent = 'Copied!';
      setTimeout(function() { btn.textContent = orig; }, 1200);
    });
  }

  copyTokenBtn.addEventListener('click', function() {
    copyToClipboard(rtmpTokenInput.value, copyTokenBtn);
  });

  regenerateBtn.addEventListener('click', function() {
    hideError(rtmpError);
    hideSuccess(rtmpSuccess);

    var ok = confirm(
      'This will disconnect all external RTMP subscribers (e.g. go2rtc). ' +
      'They will need to be reconfigured with the new token.\n\nContinue?'
    );
    if (!ok) return;

    regenerateBtn.disabled = true;
    regenerateBtn.textContent = 'Regenerating...';

    fetchJSON('/api/rtmp/token/regenerate', { method: 'POST' })
      .then(function(d) {
        currentTokenData = d;
        renderStreamUrls();
        showSuccess(rtmpSuccess, 'Token regenerated. Update any external consumers with the new URLs.');
      })
      .catch(function(err) {
        showError(rtmpError, err.message);
      })
      .finally(function() {
        regenerateBtn.disabled = false;
        regenerateBtn.textContent = 'Regenerate Token';
      });
  });

  fetch('/api/version').then(function(r) { return r.json(); }).then(function(d) {
    document.getElementById('versionFooter').textContent = 'nanit-bridge ' + (d.version || '');
  }).catch(function() {});

  refreshNanitStatus();
  loadRTMPToken();
  loadBabies();
})();
