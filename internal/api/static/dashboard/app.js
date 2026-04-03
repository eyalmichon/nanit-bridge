(function() {
  var babiesEl = document.getElementById('babies');
  var emptyState = document.getElementById('emptyState');
  var wsDot = document.getElementById('wsDot');
  var wsLabel = document.getElementById('wsLabel');
  var babies = {};
  var players = {};
  var notifSettings = {};
  var prevSensors = {};
  var ws;
  var reconnectDelay = 1000;
  var lastWsAuthProbeAt = 0;
  var nanitConnected = true;

  var nanitAuthModal = document.getElementById('nanitAuthModal');
  var nanitAuthHint = document.getElementById('nanitAuthHint');
  var nanitLoginForm = document.getElementById('nanitLoginForm');
  var nanitMfaForm = document.getElementById('nanitMfaForm');
  var nanitLoginBtn = document.getElementById('nanitLoginBtn');
  var nanitMfaBtn = document.getElementById('nanitMfaBtn');
  var nanitEmail = document.getElementById('nanitEmail');
  var nanitPassword = document.getElementById('nanitPassword');
  var nanitMfaCode = document.getElementById('nanitMfaCode');
  var nanitAuthError = document.getElementById('nanitAuthError');
  var nanitAuthSuccess = document.getElementById('nanitAuthSuccess');

  function showNanitError(msg) {
    nanitAuthError.textContent = msg || 'Request failed.';
    nanitAuthError.classList.remove('hidden');
  }

  function hideNanitError() {
    nanitAuthError.textContent = '';
    nanitAuthError.classList.add('hidden');
  }

  function showNanitSuccess(msg) {
    nanitAuthSuccess.textContent = msg || 'Saved.';
    nanitAuthSuccess.classList.remove('hidden');
  }

  function hideNanitSuccess() {
    nanitAuthSuccess.textContent = '';
    nanitAuthSuccess.classList.add('hidden');
  }

  function setNanitConnected(connected, email, mfaPending) {
    nanitConnected = !!connected;
    if (nanitConnected) {
      nanitAuthModal.classList.add('hidden');
      hideNanitError();
      hideNanitSuccess();
      nanitMfaForm.classList.add('hidden');
      nanitLoginForm.classList.remove('hidden');
      return;
    }
    if (mfaPending) {
      nanitAuthHint.textContent = 'Enter the MFA code from your authenticator app to complete Nanit login.';
      nanitLoginForm.classList.add('hidden');
      nanitMfaForm.classList.remove('hidden');
    } else {
      nanitAuthHint.textContent = email
        ? ('Nanit account (' + email + ') is disconnected. Reconnect to resume streaming and controls.')
        : 'Reconnect your Nanit account to resume camera streaming and controls.';
      nanitLoginForm.classList.remove('hidden');
      nanitMfaForm.classList.add('hidden');
    }
    nanitAuthModal.classList.remove('hidden');
  }

  function refreshNanitStatus() {
    return fetch('/api/nanit/status')
      .then(function(r) {
        if (handleAuthError(r)) return null;
        if (!r.ok) return null;
        return r.json();
      })
      .then(function(d) {
        if (!d) return;
        setNanitConnected(d.connected, d.email || '', !!d.mfa_pending);
        if (!d.connected) {
          emptyState.querySelector('h2').textContent = d.mfa_pending
            ? 'MFA verification required'
            : 'Nanit account disconnected';
          emptyState.querySelector('p').textContent = d.mfa_pending
            ? 'Enter your MFA code below to finish connecting.'
            : 'Reconnect from this modal or from /settings.';
        }
      })
      .catch(function(err) { console.warn('[nanit-status] fetch failed:', err); });
  }

  // ── WebSocket ──────────────────────────────────────────

  function scheduleReconnect() {
    wsDot.classList.remove('connected');
    wsLabel.textContent = 'reconnecting...';
    setTimeout(connect, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 10000);
  }

  function probeAuthAfterWSFailure() {
    var now = Date.now();
    if (now - lastWsAuthProbeAt < 5000) return;
    lastWsAuthProbeAt = now;

    fetch('/api/babies', { cache: 'no-store' })
      .then(function(r) {
        if (handleAuthError(r)) return;
        if (r && r.status === 503) {
          window.location.href = '/setup';
        }
      })
      .catch(function(err) { console.warn('[ws-auth-probe] fetch failed:', err); });
  }

  function connect() {
    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    var closing = false;
    try { ws = new WebSocket(proto + '//' + location.host + '/ws'); }
    catch(e) { scheduleReconnect(); return; }

    ws.onopen = function() {
      wsDot.classList.add('connected');
      wsLabel.textContent = 'live';
      reconnectDelay = 1000;
      refreshNanitStatus();
    };
    ws.onerror = function() {
      if (!closing) {
        closing = true;
        try { ws.close(); } catch(e) {}
        probeAuthAfterWSFailure();
        scheduleReconnect();
      }
    };
    ws.onclose = function() {
      if (!closing) {
        closing = true;
        probeAuthAfterWSFailure();
        scheduleReconnect();
      }
    };
    ws.onmessage = function(e) {
      var msg;
      try { msg = JSON.parse(e.data); } catch (err) { console.warn('[ws] malformed message:', err); return; }
      if (msg.type === 'initial') {
        babies = {};
        prevSensors = {};
        (msg.babies || []).forEach(function(b) { babies[b.uid] = b; });
        Object.keys(players).forEach(destroyPlayer);
        renderAll();
        refreshNanitStatus();
      } else if (msg.type === 'state_update') {
        var prev = babies[msg.baby.uid];
        babies[msg.baby.uid] = msg.baby;
        updateCard(msg.baby.uid, prev);
      } else if (msg.type === 'log_init') {
        appendLogLines(msg.lines || []);
      } else if (msg.type === 'log') {
        appendLogLine(msg.line);
      }
    };
  }

  // ── Rendering ──────────────────────────────────────────

  function renderAll() {
    var uids = Object.keys(babies);
    if (uids.length === 0) {
      emptyState.style.display = '';
      if (!nanitConnected) {
        emptyState.querySelector('h2').textContent = 'Nanit account disconnected';
        emptyState.querySelector('p').textContent = 'Reconnect from this modal or from /settings.';
      } else {
        emptyState.querySelector('h2').textContent = 'Waiting for data...';
        emptyState.querySelector('p').textContent = 'Connecting to nanit-bridge WebSocket';
      }
      return;
    }
    emptyState.style.display = 'none';
    babiesEl.querySelectorAll('.baby-card').forEach(function(el) {
      if (!babies[el.dataset.uid]) {
        destroyPlayer(el.dataset.uid);
        el.querySelectorAll('.env-num').forEach(function(num) {
          if (num._rollAnim) { cancelAnimationFrame(num._rollAnim); num._rollAnim = null; }
        });
        el.remove();
      }
    });
    uids.forEach(function(uid) { updateCard(uid, null); });
  }

  // ── API helpers ────────────────────────────────────────

  function sendControl(uid, action, value) {
    return fetch('/api/babies/' + uid + '/control', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: action, value: value })
    }).then(function(r) {
      if (handleAuthError(r)) return;
      if (!r.ok) throw new Error(r.status + ' ' + r.statusText);
      return r;
    });
  }

  function sendControlOrWarn(uid, action, value) {
    sendControl(uid, action, value).catch(function(err) {
      console.warn('[control] ' + action + ' failed:', err);
    });
  }

  function fetchNotifSettings(uid) {
    fetch('/api/babies/' + uid + '/notification_settings')
      .then(function(r) { if (handleAuthError(r)) return; return r.json(); })
      .then(function(d) {
        if (!d) return;
        notifSettings[uid] = d.settings || {};
        renderNotifToggles(uid);
      })
      .catch(function(e) { console.error('notif settings error:', e); });
  }

  function toggleNotifSetting(uid, key) {
    var cur = notifSettings[uid] && notifSettings[uid][key];
    var newVal = !cur;
    if (notifSettings[uid]) notifSettings[uid][key] = newVal;
    renderNotifToggles(uid);
    fetch('/api/babies/' + uid + '/notification_settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key: key, enabled: newVal })
    })
    .then(function(r) { if (handleAuthError(r)) return null; return r.json(); })
    .then(function(d) { if (!d) return; notifSettings[uid] = d.settings || {}; renderNotifToggles(uid); })
    .catch(function(e) { console.error('toggle notif error:', e); });
  }

  // ── Notification cards ─────────────────────────────────

  function renderNotifToggles(uid) { renderNotifCards(uid); }

  var lastSliderValues = {};

  function renderNotifCards(uid, soundSlider, motionSlider) {
    var el = document.getElementById('notif-cards-' + uid);
    if (!el) return;
    var ns = notifSettings[uid] || {};
    if (soundSlider !== undefined) lastSliderValues[uid] = { sound: soundSlider, motion: motionSlider };
    var sv = lastSliderValues[uid] || { sound: 4, motion: 12 };

    var groups = [
      { key: 'SOUND', label: 'Sound Alerts', sensId: 'ctrl-sound-sens-' + uid, sensMin: 0, sensMax: 7, sensVal: sv.sound },
      { key: 'MOTION', label: 'Motion Alerts', sensId: 'ctrl-motion-sens-' + uid, sensMin: 0, sensMax: 24, sensVal: sv.motion },
      { key: 'CAMERA_CRY_DETECTION', label: 'Cry Detection', sensId: null }
    ];

    var html = '';
    for (var i = 0; i < groups.length; i++) {
      var g = groups[i];
      var on = ns[g.key] === true;
      html += '<div class="notif-item">';
      html += '<div class="notif-head">';
      html += '<span class="ctrl-label">' + g.label + '</span>';
      html += '<button class="toggle ' + (on ? 'on' : '') + '" data-uid="' + uid + '" data-key="' + g.key + '"></button>';
      html += '</div>';
      if (g.sensId) {
        html += '<div class="notif-slider">';
        html += '<span class="sens-label">Less</span>';
        html += '<input type="range" id="' + g.sensId + '" min="' + g.sensMin + '" max="' + g.sensMax + '" value="' + g.sensVal + '">';
        html += '<span class="sens-label">More</span>';
        html += '</div>';
      }
      html += '</div>';
    }

    el.innerHTML = html;

    el.querySelectorAll('.toggle').forEach(function(btn) {
      btn.onclick = function() { toggleNotifSetting(this.dataset.uid, this.dataset.key); };
    });

    var soundSens = document.getElementById('ctrl-sound-sens-' + uid);
    if (soundSens) {
      soundSens.onchange = function() {
        sendControlOrWarn(uid, 'sound_sensitivity', 9 - parseInt(this.value));
      };
    }
    var motionSens = document.getElementById('ctrl-motion-sens-' + uid);
    if (motionSens) {
      motionSens.onchange = function() {
        sendControlOrWarn(uid, 'motion_sensitivity', 250000 - (parseInt(this.value) * 10000));
      };
    }
  }

  // ── Pending control state (optimistic UI) ──────────────

  var pendingControls = {};

  function setPending(uid, key, value) {
    if (!pendingControls[uid]) pendingControls[uid] = {};
    pendingControls[uid][key] = { value: value, ts: Date.now() };
  }

  function getPending(uid, key) {
    return pendingControls[uid] && pendingControls[uid][key];
  }

  function clearPending(uid, key) {
    if (pendingControls[uid]) delete pendingControls[uid][key];
  }

  function getControlValue(uid, key, serverValue) {
    var p = getPending(uid, key);
    if (p && (Date.now() - p.ts) < 5000) return p.value;
    if (p) clearPending(uid, key);
    return serverValue;
  }

  // ── Card lifecycle ─────────────────────────────────────

  function flashElement(el) {
    el.classList.remove('flash');
    void el.offsetWidth;
    el.classList.add('flash');
  }

  function rollValue(el, from, to, decimals, duration) {
    if (el._rollAnim) cancelAnimationFrame(el._rollAnim);
    var start = performance.now();
    var diff = to - from;
    (function frame(now) {
      var t = Math.min((now - start) / duration, 1);
      var ease = t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;
      el.textContent = (from + diff * ease).toFixed(decimals);
      if (t < 1) el._rollAnim = requestAnimationFrame(frame);
      else el._rollAnim = null;
    })(start);
  }

  function ensureCard(uid) {
    var card = babiesEl.querySelector('.baby-card[data-uid="' + uid + '"]');
    if (!card) {
      card = document.createElement('div');
      card.className = 'baby-card';
      card.dataset.uid = uid;
      card.innerHTML =
        '<div class="video-container">' +
          '<video id="video-' + uid + '" muted autoplay playsinline></video>' +
          '<div class="video-overlay" id="overlay-' + uid + '">Stream not active</div>' +
          '<button class="unmute-btn" id="unmute-' + uid + '" title="Unmute">&#128263;</button>' +
        '</div>' +
        '<div class="baby-body">' +
          '<div id="info-' + uid + '">' +
            '<div class="baby-header">' +
              '<div><h2 id="baby-name-' + uid + '"></h2>' +
              '<div class="uid" id="baby-uid-' + uid + '"></div></div>' +
              '<div class="status-pills" id="pills-' + uid + '"></div>' +
            '</div>' +
            '<div class="env-grid">' +
              '<div class="env-item"><div class="env-label">Temp</div>' +
                '<div class="env-val" id="env-temp-' + uid + '"><span class="env-num"></span><span class="env-unit">\u00b0C</span></div></div>' +
              '<div class="env-item"><div class="env-label">Humidity</div>' +
                '<div class="env-val" id="env-humidity-' + uid + '"><span class="env-num"></span><span class="env-unit">%</span></div></div>' +
              '<div class="env-item"><div class="env-label">Light</div>' +
                '<div class="env-val" id="env-light-' + uid + '"><span class="env-num"></span><span class="env-unit">lx</span></div></div>' +
            '</div>' +
            '<div class="env-extra">' +
              '<span id="env-daynight-' + uid + '"><span class="env-dot day" id="env-dot-' + uid + '"></span><span id="env-daynight-text-' + uid + '"></span></span>' +
              '<span id="env-lastupdate-' + uid + '"></span>' +
            '</div>' +
          '</div>' +
          '<div id="controls-' + uid + '"></div>' +
        '</div>';
      babiesEl.appendChild(card);
      ['env-temp-', 'env-humidity-', 'env-light-'].forEach(function(prefix) {
        var valEl = document.getElementById(prefix + uid);
        if (valEl) valEl.addEventListener('animationend', function() { this.classList.remove('flash'); });
      });
      var unmuteBtn = document.getElementById('unmute-' + uid);
      unmuteBtn.onclick = function() {
        var vid = document.getElementById('video-' + uid);
        if (vid) {
          vid.muted = !vid.muted;
          this.innerHTML = vid.muted ? '&#128263;' : '&#128266;';
          this.classList.toggle('active', !vid.muted);
        }
      };
      fetchNotifSettings(uid);
    }
    return card;
  }

  function updateCard(uid, prev) {
    var b = babies[uid];
    if (!b) return;
    emptyState.style.display = 'none';

    var card = ensureCard(uid);
    var controlsEl = document.getElementById('controls-' + uid);
    var overlay = document.getElementById('overlay-' + uid);

    var s = b.sensors || {};
    var c = b.controls || {};

    var nlOn = getControlValue(uid, 'night_light', c.night_light);
    var nlBright = c.night_light_brightness || 0;
    var nlTimeout = c.night_light_timeout || 0;
    var pbOn = getControlValue(uid, 'playback', c.playback);
    var vol = getControlValue(uid, 'volume', c.volume || 0);
    var sleepMode = c.sleep_mode || false;
    var nightVision = c.night_vision || 0;
    var statusLight = c.status_light || false;
    var micMute = c.mic_mute || false;
    var br = c.breathing || {};
    var brActive = br.active || false;
    var brCalibrating = br.calibrating || false;
    var brBpm = br.breaths_per_min || 0;
    var brStatusText = !brActive ? 'Off'
      : brCalibrating ? 'Searching for motion\u2026'
      : brBpm === 0 ? 'Scanning for breaths\u2026'
      : brBpm + ' breaths/min';

    var streamPill = b.stream === 'active'
      ? '<span class="pill pill-green">stream</span>'
      : b.stream === 'starting'
        ? '<span class="pill pill-amber">starting</span>'
        : '<span class="pill pill-red">no stream</span>';

    var wsPill = b.ws_alive
      ? '<span class="pill pill-blue">ws</span>'
      : '<span class="pill pill-red">ws off</span>';

    var pushPill = b.push_active
      ? '<span class="pill pill-green">push</span>'
      : '<span class="pill pill-amber">polling</span>';

    var lastUpdate = s.last_update && s.last_update !== '0001-01-01T00:00:00Z'
      ? new Date(s.last_update).toLocaleTimeString() : 'no data yet';

    // ── Info section (stable DOM updates) ──
    document.getElementById('baby-name-' + uid).textContent = b.name || uid;
    document.getElementById('baby-uid-' + uid).textContent = uid;

    var pillKey = (b.stream || '') + '|' + (b.ws_alive ? '1' : '0') + '|' + (b.push_active ? '1' : '0');
    var pillsEl = document.getElementById('pills-' + uid);
    if (pillsEl.dataset.state !== pillKey) {
      pillsEl.dataset.state = pillKey;
      pillsEl.innerHTML = streamPill + wsPill + pushPill;
    }

    var tempStr = fmtNum(s.temperature, 1);
    var humidStr = fmtNum(s.humidity, 1);
    var lightStr = fmtNum(s.light, 0);
    var ps = prevSensors[uid];

    var tempNumEl = document.getElementById('env-temp-' + uid).querySelector('.env-num');
    var humidNumEl = document.getElementById('env-humidity-' + uid).querySelector('.env-num');
    var lightNumEl = document.getElementById('env-light-' + uid).querySelector('.env-num');

    if (!ps || ps.temp !== tempStr) {
      if (ps && ps.tempRaw && s.temperature) rollValue(tempNumEl, ps.tempRaw, s.temperature, 1, 500);
      else tempNumEl.textContent = tempStr;
      if (ps) flashElement(tempNumEl.parentNode);
    }
    if (!ps || ps.humidity !== humidStr) {
      if (ps && ps.humidRaw && s.humidity) rollValue(humidNumEl, ps.humidRaw, s.humidity, 1, 500);
      else humidNumEl.textContent = humidStr;
      if (ps) flashElement(humidNumEl.parentNode);
    }
    if (!ps || ps.light !== lightStr) {
      if (ps && ps.lightRaw && s.light) rollValue(lightNumEl, ps.lightRaw, s.light, 0, 500);
      else lightNumEl.textContent = lightStr;
      if (ps) flashElement(lightNumEl.parentNode);
    }

    prevSensors[uid] = {
      temp: tempStr, humidity: humidStr, light: lightStr,
      tempRaw: s.temperature || 0, humidRaw: s.humidity || 0, lightRaw: s.light || 0
    };

    document.getElementById('env-dot-' + uid).className = 'env-dot ' + (s.is_night ? 'night' : 'day');
    document.getElementById('env-daynight-text-' + uid).textContent = s.is_night ? 'Nighttime' : 'Daytime';
    document.getElementById('env-lastupdate-' + uid).textContent = lastUpdate;

    // ── Controls section ──
    var tracks = c.soundtracks || [];
    var curTrack = c.current_track || '';
    var pollSec = b.sensor_poll_sec || 30;
    var soundSensRaw = c.sound_sensitivity || 5;
    var motionSensRaw = c.motion_sensitivity || 130000;
    var soundSlider = Math.max(0, Math.min(7, 9 - soundSensRaw));
    var motionSlider = Math.max(0, Math.min(24, Math.round((250000 - motionSensRaw) / 10000)));

    var cam = b.camera || {};

    var controlsChanged = !prev || !prev.controls ||
      prev.controls.night_light !== c.night_light ||
      prev.controls.playback !== c.playback ||
      prev.controls.volume !== c.volume ||
      prev.controls.current_track !== c.current_track ||
      (prev.controls.soundtracks || []).length !== tracks.length;

    if (!controlsEl.hasChildNodes() || controlsChanged) {
      renderControlsFull(uid, controlsEl, {
        nlOn: nlOn, nlBright: nlBright, nlTimeout: nlTimeout,
        pbOn: pbOn, vol: vol, tracks: tracks, curTrack: curTrack,
        brActive: brActive, brCalibrating: brCalibrating, brBpm: brBpm,
        brStatusText: brStatusText, pollSec: pollSec,
        sleepMode: sleepMode, nightVision: nightVision,
        statusLight: statusLight, micMute: micMute,
        s: s, soundSlider: soundSlider, motionSlider: motionSlider,
        cam: cam
      });
    } else {
      syncControls(uid, {
        nlOn: nlOn, nlBright: nlBright, nlTimeout: nlTimeout,
        pbOn: pbOn, curTrack: curTrack,
        brActive: brActive, brCalibrating: brCalibrating, brBpm: brBpm,
        brStatusText: brStatusText, sleepMode: sleepMode,
        nightVision: nightVision, statusLight: statusLight, micMute: micMute,
        soundSlider: soundSlider, motionSlider: motionSlider,
        s: s, cam: cam
      });
    }

    // ── Video stream management ──
    var isActive = b.rtmp_active || b.stream === 'active';
    if (isActive && !players[uid]) {
      overlay.classList.add('hidden');
      startPlayer(uid);
    } else if (!isActive && players[uid]) {
      overlay.textContent = 'Stream not active';
      overlay.classList.remove('hidden');
      destroyPlayer(uid);
    } else if (!isActive) {
      overlay.textContent = 'Stream not active';
      overlay.classList.remove('hidden');
    }
  }

  // ── Full controls render (initial + structural changes) ──

  function renderControlsFull(uid, controlsEl, d) {
    var trackOptions = '';
    for (var i = 0; i < d.tracks.length; i++) {
      var tName = d.tracks[i].name || '';
      var displayName = tName.replace(/\.wav$/i, '');
      var selected = tName === d.curTrack ? ' selected' : '';
      trackOptions += '<option value="' + esc(tName) + '"' + selected + '>' + esc(displayName) + '</option>';
    }
    var trackHtml = d.tracks.length > 0
      ? '<select class="ctrl-select" id="ctrl-track-' + uid + '">' + trackOptions + '</select>'
      : '<span class="ctrl-label">no tracks</span>';

    var nlTimerOptions =
      '<option value="0"'    + (d.nlTimeout === 0    ? ' selected' : '') + '>Always on</option>' +
      '<option value="900"'  + (d.nlTimeout === 900  ? ' selected' : '') + '>15 min</option>' +
      '<option value="1800"' + (d.nlTimeout === 1800 ? ' selected' : '') + '>30 min</option>' +
      '<option value="3600"' + (d.nlTimeout === 3600 ? ' selected' : '') + '>1 hour</option>' +
      '<option value="7200"' + (d.nlTimeout === 7200 ? ' selected' : '') + '>2 hours</option>';

    controlsEl.innerHTML =
      '<div class="section-grid">' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Night Light</span>' +
          '<button class="toggle ' + (d.nlOn ? 'on' : '') + '" id="ctrl-nl-' + uid + '"></button></div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Brightness</span>' +
            '<div class="slider-row">' +
              '<input type="range" id="ctrl-bright-' + uid + '" min="0" max="100" value="' + d.nlBright + '">' +
              '<span class="slider-val" id="ctrl-bright-val-' + uid + '">' + d.nlBright + '%</span>' +
            '</div>' +
          '</div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Timer</span>' +
            '<select class="ctrl-select" id="ctrl-nl-timer-' + uid + '">' + nlTimerOptions + '</select>' +
          '</div>' +
        '</div>' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Sound Machine</span>' +
          '<button class="toggle ' + (d.pbOn ? 'on' : '') + '" id="ctrl-pb-' + uid + '"></button></div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Track</span>' +
            trackHtml +
          '</div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Volume</span>' +
            '<div class="slider-row">' +
              '<input type="range" id="ctrl-vol-' + uid + '" min="0" max="100" value="' + d.vol + '">' +
              '<span class="slider-val" id="ctrl-vol-val-' + uid + '">' + d.vol + '</span>' +
            '</div>' +
          '</div>' +
        '</div>' +

        '<div class="sec full">' +
          '<div class="sec-head"><span class="sec-title">Alerts &amp; Detection</span></div>' +
          '<div class="alert-grid" id="alert-grid-' + uid + '">' +
            alertChip('Cry', d.s.cry_detected, d.s.cry_detected_at) +
            alertChip('Sound', d.s.sound_alert, d.s.sound_alert_at) +
            alertChip('Motion', d.s.motion_alert, d.s.motion_alert_at) +
          '</div>' +
          '<div id="notif-cards-' + uid + '" style="margin-top:0.4rem;"></div>' +
        '</div>' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Breathing Monitor</span>' +
          '<button class="toggle ' + (d.brActive ? 'on' : '') + '" id="ctrl-br-' + uid + '"></button></div>' +
          '<div id="ctrl-br-status-' + uid + '">' +
            breathingStatusHtml(d.brActive, d.brCalibrating, d.brBpm, d.brStatusText) +
          '</div>' +
        '</div>' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Camera</span>' +
          '<button class="toggle sleep-toggle ' + (d.sleepMode ? 'on' : '') + '" id="ctrl-sleep-' + uid + '" title="Camera off / privacy mode"></button></div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Privacy Mode</span>' +
            '<span class="ctrl-hint" id="ctrl-sleep-hint-' + uid + '">' + (d.sleepMode ? 'Camera is off' : 'Camera is on') + '</span>' +
          '</div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Night Vision</span>' +
            '<div class="seg-ctrl" id="ctrl-nightvision-' + uid + '">' +
              '<button class="seg-btn' + (d.nightVision === 0 ? ' active' : '') + '" data-val="0">Off</button>' +
              '<button class="seg-btn' + (d.nightVision === 1 ? ' active' : '') + '" data-val="1">Auto</button>' +
              '<button class="seg-btn' + (d.nightVision === 2 ? ' active' : '') + '" data-val="2">On</button>' +
            '</div>' +
          '</div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Status LED</span>' +
            '<button class="toggle ' + (d.statusLight ? 'on' : '') + '" id="ctrl-statuslight-' + uid + '"></button>' +
          '</div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Mic Mute</span>' +
            '<button class="toggle ' + (d.micMute ? 'on' : '') + '" id="ctrl-micmute-' + uid + '"></button>' +
          '</div>' +
          '<div class="ctrl-row">' +
            '<span class="ctrl-label">Sensor Poll</span>' +
            '<div class="slider-row">' +
              '<input type="range" id="ctrl-poll-' + uid + '" min="5" max="120" value="' + d.pollSec + '">' +
              '<span class="slider-val" id="ctrl-poll-val-' + uid + '">' + d.pollSec + 's</span>' +
            '</div>' +
          '</div>' +
        '</div>' +

        '<div class="sec">' +
          '<div class="sec-head"><span class="sec-title">Camera Info</span></div>' +
          '<div id="camera-info-' + uid + '">' +
            cameraInfoHtml(d.cam) +
          '</div>' +
        '</div>' +

      '</div>';

    wireEventHandlers(uid);
    renderNotifCards(uid, d.soundSlider, d.motionSlider);
  }

  // ── Wire event handlers after full render ──

  function wireEventHandlers(uid) {
    var nlBtn = document.getElementById('ctrl-nl-' + uid);
    if (nlBtn) nlBtn.onclick = function() {
      var newVal = !this.classList.contains('on');
      setPending(uid, 'night_light', newVal);
      sendControlOrWarn(uid, 'night_light', newVal);
      this.classList.toggle('on', newVal);
    };

    var pbBtn = document.getElementById('ctrl-pb-' + uid);
    if (pbBtn) pbBtn.onclick = function() {
      var newVal = !this.classList.contains('on');
      setPending(uid, 'playback', newVal);
      sendControlOrWarn(uid, 'playback', newVal);
      this.classList.toggle('on', newVal);
    };

    var trackSel = document.getElementById('ctrl-track-' + uid);
    if (trackSel) trackSel.onchange = function() { sendControlOrWarn(uid, 'select_track', this.value); };

    var brightSlider = document.getElementById('ctrl-bright-' + uid);
    var brightVal = document.getElementById('ctrl-bright-val-' + uid);
    if (brightSlider) {
      brightSlider.oninput = function() { brightVal.textContent = this.value + '%'; };
      brightSlider.onchange = function() { sendControlOrWarn(uid, 'night_light_brightness', parseInt(this.value)); };
    }

    var nlTimerSel = document.getElementById('ctrl-nl-timer-' + uid);
    if (nlTimerSel) nlTimerSel.onchange = function() { sendControlOrWarn(uid, 'night_light_timeout', parseInt(this.value)); };

    var volSlider = document.getElementById('ctrl-vol-' + uid);
    var volVal = document.getElementById('ctrl-vol-val-' + uid);
    if (volSlider) {
      volSlider.oninput = function() { volVal.textContent = this.value; };
      volSlider.onchange = function() {
        var v = parseInt(this.value);
        setPending(uid, 'volume', v);
        sendControlOrWarn(uid, 'volume', v);
      };
    }

    var brBtn = document.getElementById('ctrl-br-' + uid);
    if (brBtn) brBtn.onclick = function() {
      var btn = this;
      var oldVal = btn.classList.contains('on');
      var newVal = !oldVal;
      btn.classList.add('pending');
      btn.classList.toggle('on', newVal);
      var statusEl = document.getElementById('ctrl-br-status-' + uid);
      if (statusEl) statusEl.innerHTML = breathingStatusHtml(newVal, false, 0, newVal ? 'Starting\u2026' : 'Stopping\u2026');
      var revertTimer = setTimeout(function() {
        if (btn.classList.contains('pending')) {
          btn.classList.remove('pending');
          btn.classList.toggle('on', oldVal);
          if (statusEl) statusEl.innerHTML = breathingStatusHtml(oldVal, false, 0, oldVal ? '' : 'Off');
        }
      }, 8000);
      setPending(uid, 'breathing', { timer: revertTimer });
      sendControl(uid, 'breathing_monitoring', newVal).catch(function() {
        clearTimeout(revertTimer);
        btn.classList.remove('pending');
        btn.classList.toggle('on', oldVal);
        if (statusEl) statusEl.innerHTML = breathingStatusHtml(oldVal, false, 0, oldVal ? '' : 'Off');
        clearPending(uid, 'breathing');
      });
    };

    var sleepBtn = document.getElementById('ctrl-sleep-' + uid);
    if (sleepBtn) sleepBtn.onclick = function() {
      var newVal = !this.classList.contains('on');
      this.classList.toggle('on', newVal);
      var hint = document.getElementById('ctrl-sleep-hint-' + uid);
      if (hint) hint.textContent = newVal ? 'Camera is off' : 'Camera is on';
      sendControlOrWarn(uid, 'sleep_mode', newVal);
    };

    var nvCtrl = document.getElementById('ctrl-nightvision-' + uid);
    if (nvCtrl) {
      var nvBtns = nvCtrl.querySelectorAll('.seg-btn');
      nvBtns.forEach(function(btn) {
        btn.onclick = function() {
          nvBtns.forEach(function(b) { b.classList.remove('active'); });
          this.classList.add('active');
          sendControlOrWarn(uid, 'night_vision', parseInt(this.getAttribute('data-val'), 10));
        };
      });
    }

    var slBtn = document.getElementById('ctrl-statuslight-' + uid);
    if (slBtn) slBtn.onclick = function() {
      var newVal = !this.classList.contains('on');
      this.classList.toggle('on', newVal);
      sendControlOrWarn(uid, 'status_light', newVal);
    };

    var mmBtn = document.getElementById('ctrl-micmute-' + uid);
    if (mmBtn) mmBtn.onclick = function() {
      var newVal = !this.classList.contains('on');
      this.classList.toggle('on', newVal);
      sendControlOrWarn(uid, 'mic_mute', newVal);
    };

    var pollSlider = document.getElementById('ctrl-poll-' + uid);
    var pollVal = document.getElementById('ctrl-poll-val-' + uid);
    if (pollSlider) {
      pollSlider.oninput = function() { pollVal.textContent = this.value + 's'; };
      pollSlider.onchange = function() { sendControlOrWarn(uid, 'sensor_poll', parseInt(this.value)); };
    }
  }

  // ── Sync-only update (no DOM rebuild) ──

  function syncControls(uid, d) {
    var nlBtnSync = document.getElementById('ctrl-nl-' + uid);
    if (nlBtnSync) nlBtnSync.classList.toggle('on', d.nlOn);

    var brightSync = document.getElementById('ctrl-bright-' + uid);
    var brightValSync = document.getElementById('ctrl-bright-val-' + uid);
    if (brightSync) { brightSync.value = d.nlBright; brightValSync.textContent = d.nlBright + '%'; }

    var nlTimerSync = document.getElementById('ctrl-nl-timer-' + uid);
    if (nlTimerSync) nlTimerSync.value = d.nlTimeout;

    var pbBtnSync = document.getElementById('ctrl-pb-' + uid);
    if (pbBtnSync) pbBtnSync.classList.toggle('on', d.pbOn);

    var trackSync = document.getElementById('ctrl-track-' + uid);
    if (trackSync && d.curTrack) trackSync.value = d.curTrack;

    var soundSensEl = document.getElementById('ctrl-sound-sens-' + uid);
    if (soundSensEl) soundSensEl.value = d.soundSlider;

    var motionSensEl = document.getElementById('ctrl-motion-sens-' + uid);
    if (motionSensEl) motionSensEl.value = d.motionSlider;

    var brBtnSync = document.getElementById('ctrl-br-' + uid);
    if (brBtnSync) {
      var brPend = getPending(uid, 'breathing');
      if (brPend && brPend.timer) { clearTimeout(brPend.timer); clearPending(uid, 'breathing'); }
      brBtnSync.classList.remove('pending');
      brBtnSync.classList.toggle('on', d.brActive);
    }

    var brStatusSync = document.getElementById('ctrl-br-status-' + uid);
    if (brStatusSync) {
      var brKey = (d.brActive ? '1' : '0') + '|' + (d.brCalibrating ? '1' : '0') + '|' + d.brBpm;
      if (brStatusSync.dataset.state !== brKey) {
        var prevBrKey = brStatusSync.dataset.state || '';
        var prevRunning = prevBrKey.indexOf('1|0|') === 0 && prevBrKey !== '1|0|0';
        var nowRunning = d.brActive && !d.brCalibrating && d.brBpm > 0;

        if (prevRunning && nowRunning) {
          var prevBpm = parseInt(prevBrKey.split('|')[2], 10);
          var ringEl = brStatusSync.querySelector('.br-ring');
          var waveEl = brStatusSync.querySelector('.br-wave');
          var bpmEl = brStatusSync.querySelector('.br-viz-bpm');
          var dur = (60 / d.brBpm).toFixed(2);
          if (ringEl) ringEl.style.setProperty('--breath-dur', dur + 's');
          if (waveEl) waveEl.style.setProperty('--wave-dur', (dur * 1.2).toFixed(2) + 's');
          if (bpmEl) {
            rollValue(bpmEl, prevBpm, d.brBpm, 0, 400);
            flashElement(bpmEl);
          }
        } else {
          brStatusSync.innerHTML = breathingStatusHtml(d.brActive, d.brCalibrating, d.brBpm, d.brStatusText);
          var newBpmEl = brStatusSync.querySelector('.br-viz-bpm');
          if (newBpmEl) newBpmEl.addEventListener('animationend', function() { this.classList.remove('flash'); });
        }
        brStatusSync.dataset.state = brKey;
      }
    }

    var sleepSync = document.getElementById('ctrl-sleep-' + uid);
    if (sleepSync) sleepSync.classList.toggle('on', d.sleepMode);
    var sleepHintSync = document.getElementById('ctrl-sleep-hint-' + uid);
    if (sleepHintSync) sleepHintSync.textContent = d.sleepMode ? 'Camera is off' : 'Camera is on';

    var nvSync = document.getElementById('ctrl-nightvision-' + uid);
    if (nvSync) {
      var nvSyncBtns = nvSync.querySelectorAll('.seg-btn');
      nvSyncBtns.forEach(function(b) {
        b.classList.toggle('active', parseInt(b.getAttribute('data-val'), 10) === d.nightVision);
      });
    }
    var slSync = document.getElementById('ctrl-statuslight-' + uid);
    if (slSync) slSync.classList.toggle('on', d.statusLight);
    var mmSync = document.getElementById('ctrl-micmute-' + uid);
    if (mmSync) mmSync.classList.toggle('on', d.micMute);

    var alertGrid = document.getElementById('alert-grid-' + uid);
    if (alertGrid) {
      var alertKey = (d.s.cry_detected ? '1' : '0') + '|' + (d.s.sound_alert ? '1' : '0') + '|' + (d.s.motion_alert ? '1' : '0') +
        '|' + (d.s.cry_detected_at || '') + '|' + (d.s.sound_alert_at || '') + '|' + (d.s.motion_alert_at || '');
      if (alertGrid.dataset.state !== alertKey) {
        alertGrid.dataset.state = alertKey;
        alertGrid.innerHTML =
          alertChip('Cry', d.s.cry_detected, d.s.cry_detected_at) +
          alertChip('Sound', d.s.sound_alert, d.s.sound_alert_at) +
          alertChip('Motion', d.s.motion_alert, d.s.motion_alert_at);
      }
    }

    var camInfo = document.getElementById('camera-info-' + uid);
    if (camInfo) {
      var camKey = (d.cam.firmware_version || '') + '|' + (d.cam.hardware_version || '') + '|' + (d.cam.mounting_mode || '');
      if (camInfo.dataset.state !== camKey) {
        camInfo.dataset.state = camKey;
        camInfo.innerHTML = cameraInfoHtml(d.cam);
      }
    }
  }

  // ── Video player ───────────────────────────────────────

  function startPlayer(uid) {
    if (!mpegts.isSupported()) { console.warn('mpegts.js not supported'); return; }
    var videoEl = document.getElementById('video-' + uid);
    if (!videoEl) return;
    var player = mpegts.createPlayer({
      type: 'flv', isLive: true,
      url: location.origin + '/api/stream/' + uid
    }, {
      enableWorker: true,
      liveBufferLatencyChasing: true,
      liveBufferLatencyMaxLatency: 3.0,
      liveBufferLatencyMinRemain: 0.5,
      lazyLoadMaxDuration: 5,
      deferLoadAfterSourceOpen: false,
      autoCleanupSourceBuffer: true,
      autoCleanupMaxBackwardDuration: 10,
      autoCleanupMinBackwardDuration: 5
    });
    player.attachMediaElement(videoEl);
    player.load();
    player.play();
    var errorTimer = null;
    player.on(mpegts.Events.ERROR, function() {
      errorTimer = setTimeout(function() {
        errorTimer = null;
        destroyPlayer(uid);
        if (babies[uid] && babies[uid].stream === 'active') startPlayer(uid);
      }, 3000);
    });
    var stallCount = 0;
    var onStall = function() {
      stallCount++;
      if (stallCount > 3) {
        destroyPlayer(uid);
        if (babies[uid] && babies[uid].stream === 'active') startPlayer(uid);
      }
    };
    videoEl.addEventListener('stalled', onStall);
    players[uid] = { player: player, errorTimer: function() { return errorTimer; }, onStall: onStall, videoEl: videoEl };
  }

  document.addEventListener('visibilitychange', function() {
    if (document.visibilityState !== 'visible') return;
    Object.keys(players).forEach(function(uid) {
      var video = document.getElementById('video-' + uid);
      if (!video || !video.buffered || video.buffered.length === 0) return;
      var bufferedEnd = video.buffered.end(video.buffered.length - 1);
      var latency = bufferedEnd - video.currentTime;
      if (latency > 5) {
        destroyPlayer(uid);
        var b = babies[uid];
        if (b && (b.rtmp_active || b.stream === 'active')) {
          startPlayer(uid);
        }
      }
    });
  });

  function destroyPlayer(uid) {
    var p = players[uid];
    if (p) {
      var timer = p.errorTimer();
      if (timer) clearTimeout(timer);
      if (p.videoEl && p.onStall) p.videoEl.removeEventListener('stalled', p.onStall);
      try { p.player.pause(); p.player.unload(); p.player.detachMediaElement(); p.player.destroy(); } catch(e) {}
      delete players[uid];
    }
  }

  // ── HTML helpers ───────────────────────────────────────

  function cameraInfoHtml(cam) {
    if (!cam) cam = {};
    var fw = cam.firmware_version || '--';
    var hw = cam.hardware_version || '--';
    var mount = cam.mounting_mode || '--';
    return '<div class="info-grid">' +
      infoRow('Firmware', fw) +
      infoRow('Hardware', hw) +
      infoRow('Mount', mount) +
    '</div>';
  }

  function infoRow(label, value) {
    return '<div class="ctrl-row"><span class="ctrl-label">' + label + '</span><span class="ctrl-val">' + esc(value) + '</span></div>';
  }

  var BR_WAVE_SVG =
    '<svg viewBox="0 0 200 40" preserveAspectRatio="none">' +
      '<path d="M0 20 Q25 0 50 20 T100 20 T150 20 T200 20" fill="none" stroke="currentColor" stroke-width="3"/>' +
    '</svg>' +
    '<svg viewBox="0 0 200 40" preserveAspectRatio="none">' +
      '<path d="M0 22 Q25 4 50 22 T100 22 T150 22 T200 22" fill="none" stroke="currentColor" stroke-width="2"/>' +
    '</svg>';

  function breathingStatusHtml(active, calibrating, bpm, statusText) {
    if (!active) return '<span class="br-detail">Off</span>';
    if (calibrating || bpm === 0) {
      return '<div class="br-viz">' +
        '<div class="br-ring calibrating">' +
          '<span class="br-viz-label">' + esc(statusText) + '</span>' +
        '</div>' +
      '</div>';
    }
    var dur = (60 / bpm).toFixed(2);
    var waveDur = (dur * 1.2).toFixed(2);
    return '<div class="br-viz">' +
      '<div class="br-ring" style="--breath-dur:' + dur + 's">' +
        '<div class="br-wave" style="--wave-dur:' + waveDur + 's">' + BR_WAVE_SVG + '</div>' +
        '<span class="br-viz-bpm">' + bpm + '</span>' +
        '<span class="br-viz-unit">breaths/min</span>' +
      '</div>' +
    '</div>';
  }

  function alertChip(label, detected, detectedAt) {
    var active = false;
    var ago = '';
    if (detected && detectedAt && detectedAt !== '0001-01-01T00:00:00Z') {
      var elapsed = (Date.now() - new Date(detectedAt).getTime()) / 1000;
      active = elapsed < 60;
      if (elapsed < 10) ago = ' just now';
      else if (elapsed < 60) ago = ' ' + Math.round(elapsed) + 's';
      else if (elapsed < 3600) ago = ' ' + Math.round(elapsed / 60) + 'm';
    }
    return '<div class="alert-chip' + (active ? ' active' : '') + '">' +
      '<span class="dot"></span>' + label + ago + '</div>';
  }

  function fmtNum(v, decimals) {
    if (v == null || v === 0) return '--';
    return Number(v).toFixed(decimals);
  }

  function esc(s) {
    if (!s) return '';
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  // ── Log panel ──────────────────────────────────────────

  var logContent = document.getElementById('logContent');
  var logBody = document.getElementById('logBody');
  var logPanel = document.getElementById('logPanel');
  var logBadge = document.getElementById('logBadge');
  var logHeader = document.getElementById('logHeader');
  var logLineCount = 0;
  var LOG_MAX = 500;

  logHeader.addEventListener('click', function() {
    logPanel.classList.toggle('collapsed');
    if (!logPanel.classList.contains('collapsed')) {
      logBody.scrollTop = logBody.scrollHeight;
    }
  });

  function appendLogLine(line) {
    logLineCount++;
    logContent.appendChild(document.createTextNode(line + '\n'));
    while (logContent.childNodes.length > LOG_MAX) {
      logContent.removeChild(logContent.firstChild);
      logLineCount--;
    }
    logBadge.textContent = logLineCount + ' lines';
    var atBottom = logBody.scrollHeight - logBody.scrollTop - logBody.clientHeight < 60;
    if (atBottom) logBody.scrollTop = logBody.scrollHeight;
  }

  function appendLogLines(lines) {
    logContent.textContent = '';
    var frag = document.createDocumentFragment();
    for (var i = 0; i < lines.length; i++) {
      frag.appendChild(document.createTextNode(lines[i] + '\n'));
    }
    logContent.appendChild(frag);
    logLineCount = lines.length;
    logBadge.textContent = logLineCount + ' lines';
    logBody.scrollTop = logBody.scrollHeight;
  }

  // ── Auth ────────────────────────────────────────────────

  function handleAuthError(r) {
    if (r && r.status === 401) {
      window.location.href = '/login';
      return true;
    }
    if (r && r.status === 503) {
      refreshNanitStatus();
    }
    return false;
  }

  nanitLoginForm.addEventListener('submit', function(e) {
    e.preventDefault();
    hideNanitError();
    hideNanitSuccess();
    nanitLoginBtn.disabled = true;
    nanitLoginBtn.textContent = 'Connecting...';

    fetch('/api/nanit/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        email: nanitEmail.value,
        password: nanitPassword.value
      })
    }).then(function(r) {
      if (handleAuthError(r)) return null;
      if (!r.ok) return r.text().then(function(t) { throw new Error(t || 'Login failed'); });
      return r.json();
    }).then(function(d) {
      if (!d) return;
      if (d.status === 'mfa_required') {
        nanitMfaForm.classList.remove('hidden');
        showNanitSuccess('MFA required. Enter the code from your phone.');
        return;
      }
      nanitMfaForm.classList.add('hidden');
      showNanitSuccess('Connected successfully.');
      setNanitConnected(true, nanitEmail.value);
      refreshNanitStatus();
    }).catch(function(err) {
      showNanitError(err.message || 'Login failed.');
    }).finally(function() {
      nanitLoginBtn.disabled = false;
      nanitLoginBtn.textContent = 'Connect';
    });
  });

  nanitMfaForm.addEventListener('submit', function(e) {
    e.preventDefault();
    hideNanitError();
    hideNanitSuccess();
    nanitMfaBtn.disabled = true;
    nanitMfaBtn.textContent = 'Verifying...';

    fetch('/api/nanit/mfa', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code: nanitMfaCode.value })
    }).then(function(r) {
      if (handleAuthError(r)) return null;
      if (!r.ok) return r.text().then(function(t) { throw new Error(t || 'MFA failed'); });
      return r.json();
    }).then(function(d) {
      if (!d) return;
      nanitMfaForm.classList.add('hidden');
      nanitMfaCode.value = '';
      setNanitConnected(true, nanitEmail.value);
      showNanitSuccess('Connected successfully.');
      refreshNanitStatus();
    }).catch(function(err) {
      showNanitError(err.message || 'MFA failed.');
    }).finally(function() {
      nanitMfaBtn.disabled = false;
      nanitMfaBtn.textContent = 'Verify MFA';
    });
  });

  document.getElementById('logoutBtn').onclick = function() {
    fetch('/api/auth/logout', { method: 'POST' }).finally(function() {
      window.location.href = '/login';
    });
  };

  // ── Boot ───────────────────────────────────────────────

  fetch('/api/version').then(function(r) { return r.json(); }).then(function(d) {
    var v = d.version || '';
    var el = document.getElementById('headerVersion');
    if (el && v) {
      el.textContent = v;
      el.href = 'https://github.com/eyalmichon/nanit-bridge/releases/tag/' + encodeURIComponent(v);
    }
  }).catch(function(err) { console.warn('[version] fetch failed:', err); });

  refreshNanitStatus();
  setInterval(refreshNanitStatus, 30000);
  connect();
})();
