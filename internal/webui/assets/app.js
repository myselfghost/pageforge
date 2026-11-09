(function () {
  'use strict';

  var TOKEN_KEY = 'pageforge.admin.token';

  var state = {
    token: '',
    aliases: [],
    projects: [],
    bindings: [],
    authPolicies: [],
    shares: [],
    metrics: null,
    versions: {}
  };

  var VIEWS = ['overview', 'aliases', 'projects', 'bindings', 'auth', 'shares', 'deployments', 'monitoring'];
  var activeView = 'overview';
  var appliedHash = null;
  var detailProjectId = null;

  var $ = function (id) { return document.getElementById(id); };

  function esc(value) {
    return String(value == null ? '' : value)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function shortId(id) {
    if (!id) return '—';
    return id.length > 18 ? id.slice(0, 10) + '…' + id.slice(-4) : id;
  }

  function bytes(n) {
    if (!n && n !== 0) return '—';
    var units = ['B', 'KB', 'MB', 'GB', 'TB'];
    var i = 0;
    var v = Number(n);
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v : v.toFixed(1)) + ' ' + units[i];
  }

  function duration(sec) {
    sec = Number(sec || 0);
    if (sec < 60) return sec + 's';
    var m = Math.floor(sec / 60);
    if (m < 60) return m + 'm ' + (sec % 60) + 's';
    var h = Math.floor(m / 60);
    return h + 'h ' + (m % 60) + 'm';
  }

  function headersText(list) {
    return (list || []).map(function (h) { return h.name + ': ' + h.value; }).join('\n');
  }

  function retentionText(p) {
    if (p.keep_versions == null && p.keep_days == null) return '继承全局';
    var parts = [];
    parts.push('数量 ' + (p.keep_versions == null ? '继承' : p.keep_versions));
    parts.push('天数 ' + (p.keep_days == null ? '继承' : p.keep_days));
    return parts.join(' / ');
  }

  function parseHeaders(text) {
    var out = [];
    String(text || '').split(/\r?\n/).forEach(function (line, index) {
      var trimmed = line.trim();
      if (!trimmed || trimmed.charAt(0) === '#') return;
      var idx = trimmed.indexOf(':');
      if (idx <= 0) throw new Error('第 ' + (index + 1) + ' 行响应头格式应为 Name: Value');
      out.push({ name: trimmed.slice(0, idx).trim(), value: trimmed.slice(idx + 1).trim() });
    });
    return out;
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () {
        showToast('已复制到剪贴板');
      }).catch(function () { showToast('复制失败，请手动选择', true); });
      return;
    }
    var area = document.createElement('textarea');
    area.value = text;
    document.body.appendChild(area);
    area.select();
    try { document.execCommand('copy'); showToast('已复制到剪贴板'); } catch (e) { showToast('复制失败，请手动选择', true); }
    document.body.removeChild(area);
  }

  // ----- api -----

  function ApiError(message, status, payload) {
    this.name = 'ApiError';
    this.message = message;
    this.status = status;
    this.payload = payload;
  }
  ApiError.prototype = Object.create(Error.prototype);

  function api(method, path, body, contentType) {
    var headers = {};
    if (state.token) headers['Authorization'] = 'Bearer ' + state.token;
    var payload;
    if (body !== undefined && body !== null) {
      if (contentType) {
        headers['Content-Type'] = contentType;
        payload = body;
      } else {
        headers['Content-Type'] = 'application/json';
        payload = JSON.stringify(body);
      }
    }
    // 使用相对路径，便于控制台整体挂载在反向代理的子路径下
    return fetch('api/v1' + path, { method: method, headers: headers, body: payload })
      .then(function (res) {
        return res.text().then(function (text) {
          var data = null;
          if (text) {
            try { data = JSON.parse(text); } catch (e) { data = text; }
          }
          if (res.ok) return data;
          if (res.status === 401) {
            setOnline(false);
            throw new ApiError('管理 Token 无效或已过期，请重新配置', 401, data);
          }
          var msg = 'HTTP ' + res.status;
          if (data && data.error) {
            msg = data.error.message || data.error.code || msg;
          }
          throw new ApiError(msg, res.status, data);
        });
      });
  }

  function run(promise) {
    return promise.catch(function (err) {
      showToast(err.message || String(err), true);
      throw err;
    });
  }

  // ----- chrome -----

  function showToast(message, isError) {
    var toast = $('toast');
    toast.textContent = message;
    toast.className = 'toast' + (isError ? ' err' : '');
    clearTimeout(showToast.timer);
    var delay = isError ? 10000 : 3600;
    showToast.timer = setTimeout(function () { toast.className = 'toast hidden'; }, delay);
  }

  function setOnline(ok) {
    $('conn-dot').className = 'dot ' + (ok ? 'online' : 'offline');
    $('conn-text').textContent = ok ? '已连接' : '未连接';
  }

  function checkHealth() {
    return fetch('healthz')
      .then(function (res) { setOnline(res.ok); return res.ok; })
      .catch(function () { setOnline(false); return false; });
  }

  function showView(name, opts) {
    opts = opts || {};
    activeView = name;
    var tabs = document.querySelectorAll('.tab');
    for (var i = 0; i < tabs.length; i++) {
      // 项目详情视图在「项目管理」tab 下高亮
      var tabName = name === 'project-detail' ? 'projects' : name;
      tabs[i].classList.toggle('active', tabs[i].dataset.view === tabName);
    }
    var views = document.querySelectorAll('.view');
    for (var j = 0; j < views.length; j++) {
      views[j].classList.toggle('active', views[j].id === 'view-' + name);
    }
    if (!opts.fromRoute) routeTo('#/' + name);
    if (opts.skipLoad) return;
    if (name === 'overview') loadOverview();
    if (name === 'aliases') loadAliases();
    if (name === 'projects') loadProjects();
    if (name === 'project-detail') return; // 由 openProjectDetail 负责渲染
    if (name === 'bindings') loadBindings();
    if (name === 'auth') loadAuthPolicies();
    if (name === 'shares') loadShares();
    if (name === 'deployments') loadDeployments();
    if (name === 'monitoring') loadMonitoring();
  }

  // ----- 路由（hash）-----

  function routeTo(hash) {
    appliedHash = hash;
    if (location.hash !== hash) location.hash = hash;
  }

  function applyRoute() {
    if (location.hash === appliedHash) return;
    var raw = (location.hash || '').replace(/^#\/?/, '');
    var parts = raw.split('/').filter(function (p) { return p; });
    var name = VIEWS.indexOf(parts[0]) >= 0 ? parts[0] : 'overview';
    var detailId = name === 'projects' && parts[1] ? decodeURIComponent(parts[1]) : '';
    appliedHash = location.hash;
    if (detailId) {
      // 项目详情：切到独立视图，不弹 modal
      detailProjectId = detailId;
      showView('project-detail', { fromRoute: true, skipLoad: true });
      loadProjects().then(function () {
        var found = state.projects.some(function (p) { return p.id === detailId; });
        if (found) openProjectDetail(detailId);
        else showToast('项目不存在或已删除', true);
      });
      return;
    }
    if (detailProjectId) {
      detailProjectId = null;
    }
    showView(name, { fromRoute: true });
  }

  // ----- modal -----

  function openModal(opts) {
    $('modal-title').textContent = opts.title || '';
    $('modal-body').innerHTML = opts.body || '';
    $('modal-foot').innerHTML = opts.footer || '';
    $('modal').className = 'modal' + (opts.wide ? ' wide' : '');
    $('modal-backdrop').classList.remove('hidden');
  }

  function closeModal() {
    $('modal-backdrop').classList.add('hidden');
    $('modal-body').innerHTML = '';
    $('modal-foot').innerHTML = '';
  }

  // ----- 列表公共能力：搜索 + 分页 -----

  var listState = {};

  function listCfg(key) {
    if (!listState[key]) listState[key] = { query: '', page: 1, pageSize: 10 };
    return listState[key];
  }

  function searchJoin() {
    return Array.prototype.map.call(arguments, function (v) {
      return v == null ? '' : String(v);
    }).join('\n');
  }

  // 绑定某个列表的搜索框与每页条数选择器，输入变化时重置到第 1 页并重绘
  function wireListControls(key, rerender) {
    var search = $('search-' + key);
    var size = $('ps-' + key);
    if (!search || !size) return;
    var cfg = listCfg(key);
    search.value = cfg.query;
    size.value = String(cfg.pageSize);
    search.oninput = function () { cfg.query = search.value; cfg.page = 1; rerender(); };
    size.onchange = function () { cfg.pageSize = parseInt(size.value, 10) || 10; cfg.page = 1; rerender(); };
  }

  function pageNumbers(page, pages) {
    var i;
    if (pages <= 7) {
      var all = [];
      for (i = 1; i <= pages; i++) all.push(i);
      return all;
    }
    var nums = [];
    [1, 2, page - 1, page, page + 1, pages - 1, pages].forEach(function (n) {
      if (n >= 1 && n <= pages && nums.indexOf(n) < 0) nums.push(n);
    });
    nums.sort(function (a, b) { return a - b; });
    var out = [];
    for (i = 0; i < nums.length; i++) {
      if (i > 0 && nums[i] - nums[i - 1] > 1) out.push('…');
      out.push(nums[i]);
    }
    return out;
  }

  function renderPager(key, pagerId, total, rerender) {
    var cfg = listCfg(key);
    var pager = $(pagerId);
    if (!pager) return;
    var pages = Math.max(1, Math.ceil(total / cfg.pageSize));
    if (cfg.page > pages) cfg.page = pages;
    if (!total) {
      pager.innerHTML = '';
      pager.classList.add('hidden');
      return;
    }
    pager.classList.remove('hidden');
    var html = '<span class="page-info">共 ' + total + ' 条 · 第 ' + cfg.page + ' / ' + pages + ' 页</span><div class="page-btns">';
    html += '<button class="btn ghost small" data-p="prev"' + (cfg.page <= 1 ? ' disabled' : '') + '>上一页</button>';
    pageNumbers(cfg.page, pages).forEach(function (n) {
      if (n === '…') {
        html += '<span class="page-ellipsis">…</span>';
      } else {
        html += '<button class="btn ghost small page-num' + (n === cfg.page ? ' active' : '') + '" data-p="' + n + '">' + n + '</button>';
      }
    });
    html += '<button class="btn ghost small" data-p="next"' + (cfg.page >= pages ? ' disabled' : '') + '>下一页</button></div>';
    pager.innerHTML = html;
    Array.prototype.forEach.call(pager.querySelectorAll('[data-p]'), function (btn) {
      btn.onclick = function () {
        var p = btn.dataset.p;
        if (p === 'prev') cfg.page -= 1;
        else if (p === 'next') cfg.page += 1;
        else cfg.page = parseInt(p, 10);
        rerender();
      };
    });
  }

  // 通用列表渲染：opts = { key, tbody, empty, pager, items, searchText, row, emptyText }
  function renderList(opts) {
    var cfg = listCfg(opts.key);
    var q = cfg.query.trim().toLowerCase();
    var items = opts.items || [];
    var filtered = q
      ? items.filter(function (it) { return opts.searchText(it).toLowerCase().indexOf(q) >= 0; })
      : items;
    var pages = Math.max(1, Math.ceil(filtered.length / cfg.pageSize));
    if (cfg.page > pages) cfg.page = pages;
    var start = (cfg.page - 1) * cfg.pageSize;
    var tbody = $(opts.tbody);
    tbody.innerHTML = '';
    filtered.slice(start, start + cfg.pageSize).forEach(function (it) {
      tbody.appendChild(opts.row(it));
    });
    var empty = $(opts.empty);
    empty.classList.toggle('hidden', filtered.length > 0);
    empty.textContent = items.length && !filtered.length
      ? '未找到匹配「' + cfg.query.trim() + '」的结果'
      : opts.emptyText;
    renderPager(opts.key, opts.pager, filtered.length, function () { renderList(opts); });
  }

  // ----- overview -----

  function loadOverview() {
    checkHealth();
    $('info-api').textContent = new URL('api/v1', location.href).href;
    $('info-health').textContent = '检查中…';
    $('info-loaded').textContent = new Date().toLocaleString();

    if (!state.token) {
      $('info-health').textContent = '未配置 Token';
      return;
    }
    Promise.all([
      api('GET', '/aliases'),
      api('GET', '/projects'),
      api('GET', '/bindings'),
      api('GET', '/metrics')
    ]).then(function (results) {
      state.aliases = results[0] || [];
      state.projects = results[1] || [];
      state.bindings = results[2] || [];
      state.metrics = results[3] || {};
      $('stat-aliases').textContent = state.aliases.length;
      $('stat-projects').textContent = state.projects.length;
      $('stat-bindings').textContent = state.bindings.filter(function (b) { return b.enabled; }).length;
      $('stat-requests').textContent = state.metrics.total_requests || 0;
      $('info-health').textContent = '正常';
    }).catch(function (err) {
      $('info-health').textContent = '异常';
      if (err.status === 401) openSettings();
      showToast(err.message, true);
    });
  }

  // ----- aliases -----

  function loadAliases() {
    return run(api('GET', '/aliases')).then(function (items) {
      state.aliases = items || [];
      renderAliases();
    }).catch(function () {});
  }

  function renderAliases() {
    renderList({
      key: 'aliases',
      tbody: 'tbody-aliases',
      empty: 'empty-aliases',
      pager: 'pager-aliases',
      items: state.aliases,
      emptyText: '暂无别名',
      searchText: function (a) {
        return searchJoin(a.name, a.id, a.endpoint, a.region, (a.bucket_allowlist || []).join(' '));
      },
      row: function (a) {
        var buckets = (a.bucket_allowlist || []).join(', ') || '—';
        var tr = document.createElement('tr');
        tr.innerHTML =
          '<td><strong>' + esc(a.name) + '</strong><div class="mono">' + esc(shortId(a.id)) + '</div></td>' +
          '<td class="mono ellipsis" title="' + esc(a.endpoint) + '">' + esc(a.endpoint) + '</td>' +
          '<td>' + esc(a.region || '—') + '</td>' +
          '<td class="mono ellipsis" title="' + esc(buckets) + '">' + esc(buckets) + '</td>' +
          '<td>' + (a.secret_key_set ? '<span class="badge ok">已设置</span>' : '<span class="badge err">缺失</span>') + '</td>' +
          '<td class="right"><div class="row-actions">' +
          '<button class="btn secondary small" data-act="test">测试</button>' +
          '<button class="btn secondary small" data-act="edit">编辑</button>' +
          '<button class="btn danger small" data-act="delete">删除</button>' +
          '</div></td>';
        tr.querySelector('[data-act="test"]').onclick = function () {
          run(api('POST', '/aliases/' + a.id + '/test', {})).then(function (res) {
            showToast('连接成功：' + (res.bucket || ''));
          }).catch(function () {});
        };
        tr.querySelector('[data-act="edit"]').onclick = function () { openAliasModal(a); };
        tr.querySelector('[data-act="delete"]').onclick = function () {
          if (!confirm('确认删除别名「' + a.name + '」？')) return;
          run(api('DELETE', '/aliases/' + a.id)).then(function () {
            showToast('已删除别名');
            loadAliases();
          }).catch(function () {});
        };
        return tr;
      }
    });
  }

  function openAliasModal(existing) {
    var a = existing || {};
    var allowlist = (a.bucket_allowlist || []).join(', ');
    openModal({
      title: existing ? '编辑别名' : '新建别名',
      body:
        '<div class="form">' +
        '<div class="grid2">' +
        '<label>名称<input id="f-name" value="' + esc(a.name || '') + '" placeholder="minio-prod"></label>' +
        '<label>Endpoint<input id="f-endpoint" value="' + esc(a.endpoint || '') + '" placeholder="http://10.0.0.1:9000"></label>' +
        '</div>' +
        '<div class="grid2">' +
        '<label>Region<input id="f-region" value="' + esc(a.region || 'us-east-1') + '"></label>' +
        '<label>Bucket 白名单（逗号分隔）<input id="f-buckets" value="' + esc(allowlist) + '" placeholder="page-bucket-1"></label>' +
        '</div>' +
        '<div class="grid2">' +
        '<label>Access Key<input id="f-access" value="' + esc(a.access_key || '') + '"></label>' +
        '<label>Secret Key<input id="f-secret" type="password" placeholder="' + (existing && a.secret_key_set ? '留空表示不修改' : '') + '"></label>' +
        '</div>' +
        '<div class="grid2">' +
        '<label class="inline"><input type="checkbox" id="f-path" ' + (a.path_style !== false ? 'checked' : '') + '> Path-style 寻址</label>' +
        '<label class="inline"><input type="checkbox" id="f-tls" ' + (a.tls_skip_verify ? 'checked' : '') + '> 跳过 TLS 校验</label>' +
        '</div>' +
        '<div class="form-error" id="f-error"></div>' +
        '</div>',
      footer:
        '<button class="btn ghost" id="m-cancel">取消</button>' +
        '<button class="btn" id="m-save">保存</button>'
    });
    $('m-cancel').onclick = closeModal;
    $('m-save').onclick = function () {
      var name = $('f-name').value.trim();
      var endpoint = $('f-endpoint').value.trim();
      var secret = $('f-secret').value.trim();
      if (!name || !endpoint) {
        $('f-error').textContent = '名称与 Endpoint 必填';
        return;
      }
      var payload = {
        name: name,
        endpoint: endpoint,
        region: $('f-region').value.trim(),
        access_key: $('f-access').value.trim(),
        path_style: $('f-path').checked,
        tls_skip_verify: $('f-tls').checked,
        bucket_allowlist: $('f-buckets').value.split(',').map(function (s) { return s.trim(); }).filter(Boolean)
      };
      if (secret) payload.secret_key = secret;
      if (!existing && !secret) {
        $('f-error').textContent = '新建别名必须填写 Secret Key';
        return;
      }
      var req = existing ? api('PATCH', '/aliases/' + existing.id, payload) : api('POST', '/aliases', payload);
      run(req).then(function () {
        closeModal();
        showToast(existing ? '别名已更新' : '别名已创建');
        loadAliases();
      }).catch(function (err) {
        $('f-error').textContent = err.message;
      });
    };
  }

  // ----- projects -----

  function aliasName(id) {
    var found = state.aliases.filter(function (a) { return a.id === id; })[0];
    return found ? found.name : shortId(id);
  }

  function loadProjects() {
    return run(Promise.all([api('GET', '/projects'), api('GET', '/aliases')]).then(function (res) {
      state.projects = res[0] || [];
      state.aliases = res[1] || [];
      renderProjects();
    })).catch(function () {});
  }

  function renderProjects() {
    renderList({
      key: 'projects',
      tbody: 'tbody-projects',
      empty: 'empty-projects',
      pager: 'pager-projects',
      items: state.projects,
      emptyText: '暂无项目',
      searchText: function (p) {
        return searchJoin(p.name, p.id, p.description, p.bucket, aliasName(p.alias_id));
      },
      row: function (p) {
        var features = [];
        if (p.try_files_enabled) features.push('try_files');
        var spaWarning = '';
        if (p.spa && p.spa.enabled) {
          if (p.try_files_enabled) {
            features.push('SPA');
          } else {
            spaWarning = '<span class="badge warn">SPA 未生效（需 try_files）</span>';
          }
        }
        if (p.package_retention) features.push('保留包');
        if (p.keep_versions != null || p.keep_days != null) {
          var parts = [];
          if (p.keep_versions != null) parts.push('版本 ' + p.keep_versions);
          if (p.keep_days != null) parts.push('天数 ' + p.keep_days);
          features.push('保留策略(' + parts.join('/') + ')');
        }
        var featureBadges = features.map(function (f) { return '<span class="badge mute">' + f + '</span>'; }).join(' ');
        if (featureBadges && spaWarning) featureBadges += ' ';
        var featureCell = featureBadges + spaWarning;
        var tr = document.createElement('tr');
        tr.innerHTML =
          '<td><strong>' + esc(p.name) + '</strong><div class="mono ellipsis" title="' + esc(p.description || p.id) + '">' + esc(p.description || shortId(p.id)) + '</div></td>' +
          '<td>' + esc(aliasName(p.alias_id)) + '</td>' +
          '<td class="mono ellipsis" title="' + esc(p.bucket) + '">' + esc(p.bucket) + '</td>' +
          '<td class="mono">' + esc(shortId(p.current_version_id)) + '</td>' +
          '<td>' + (featureCell || '<span class="badge mute">默认</span>') + '</td>' +
          '<td class="right"><div class="row-actions">' +
          '<button class="btn secondary small" data-act="detail">详情</button>' +
          '<button class="btn secondary small" data-act="edit">编辑</button>' +
          '<button class="btn danger small" data-act="delete">删除</button>' +
          '</div></td>';
        tr.querySelector('[data-act="detail"]').onclick = function () { openProjectDetail(p.id); };
        tr.querySelector('[data-act="edit"]').onclick = function () { openProjectModal(p); };
        tr.querySelector('[data-act="delete"]').onclick = function () {
          if (!confirm('删除项目「' + p.name + '」会同时移除其绑定，确认继续？')) return;
          run(api('DELETE', '/projects/' + p.id)).then(function () {
            showToast('项目已删除');
            loadProjects();
          }).catch(function () {});
        };
        return tr;
      }
    });
  }

  function openProjectModal(existing) {
    var p = existing || {};
    if (!state.aliases.length) {
      showToast('请先创建对象存储别名', true);
      showView('aliases');
      return;
    }
    var options = state.aliases.map(function (a) {
      return '<option value="' + esc(a.id) + '"' + (p.alias_id === a.id ? ' selected' : '') + '>' + esc(a.name) + '</option>';
    }).join('');
    var lock = existing ? ' disabled' : '';
    openModal({
      title: existing ? '编辑项目' : '新建项目',
      body:
        '<div class="form">' +
        (existing ? '<div class="section-note">别名与 Bucket 在项目创建后不可修改。</div>' : '') +
        (existing && p.spa && p.spa.enabled && !p.try_files_enabled ? '<div class="section-note">当前配置中 SPA 已开启但 try_files 未开启，SPA 实际不会生效；保存前请先勾选 try_files。</div>' : '') +
        '<div class="grid2">' +
        '<label>名称<input id="p-name" value="' + esc(p.name || '') + '" placeholder="my-site"></label>' +
        '<label>存储<select id="p-alias"' + lock + '>' + options + '</select></label>' +
        '</div>' +
        '<label>描述<input id="p-desc" value="' + esc(p.description || '') + '"></label>' +
        '<div class="grid2">' +
        '<label>Bucket<input id="p-bucket" value="' + esc(p.bucket || '') + '" placeholder="page-bucket-1"' + lock + '></label>' +
        '<label>index 文档<input id="p-index" value="' + esc(p.index_doc || 'index.html') + '"></label>' +
        '</div>' +
        '<div class="grid2">' +
        '<label>404 文档<input id="p-error" value="' + esc(p.error_doc || '') + '" placeholder="留空则继承发布包配置"></label>' +
        '<div></div>' +
        '</div>' +
        '<p class="hint">404 文档留空表示不单独设置：优先使用发布包 pageforge.json 中的 error_doc，包内也没有时返回 JSON 格式 404；填写后以项目配置为准。</p>' +
        '<div class="grid2">' +
        '<label class="inline"><input type="checkbox" id="p-try" ' + (p.try_files_enabled ? 'checked' : '') + '> 启用 try_files（目录索引、尾斜杠 301）</label>' +
        '<label class="inline"><input type="checkbox" id="p-spa" ' + (p.spa && p.spa.enabled ? 'checked' : '') + '> 启用 SPA 回退（依赖 try_files）</label>' +
        '</div>' +
        '<p class="hint">SPA 回退是 try_files 流程中的一步：未启用 try_files 时 SPA 配置不会生效。勾选 SPA 会自动启用 try_files，取消 try_files 会自动关闭 SPA。</p>' +
        '<label class="inline"><input type="checkbox" id="p-retain" ' + (p.package_retention ? 'checked' : '') + '> 保留原始发布包</label>' +
        '<div class="grid2">' +
        '<label>版本保留数量<input id="p-keep-versions" type="number" min="0" value="' + (p.keep_versions != null ? p.keep_versions : '') + '" placeholder="留空继承全局"></label>' +
        '<label>版本保留天数<input id="p-keep-days" type="number" min="0" value="' + (p.keep_days != null ? p.keep_days : '') + '" placeholder="留空继承全局"></label>' +
        '</div>' +
        '<p class="hint">留空表示继承全局 gc.keep_versions / gc.keep_days；填写则以项目设置为准（0 表示不按该维度保留；两者都为 0 表示不删除任何版本）。</p>' +
        '<label>响应头（每行 Name: Value，支持 $project_id/$version_id）<textarea id="p-headers" rows="3" placeholder="X-Frame-Options: DENY">' + esc(headersText(p.response_headers)) + '</textarea></label>' +
        '<div class="form-error" id="p-error-msg"></div>' +
        '</div>',
      footer:
        '<button class="btn ghost" id="m-cancel">取消</button>' +
        '<button class="btn" id="m-save">保存</button>'
    });
    $('m-cancel').onclick = closeModal;
    var tryFilesEl = $('p-try');
    var spaEl = $('p-spa');
    spaEl.onchange = function () {
      if (spaEl.checked) tryFilesEl.checked = true;
    };
    tryFilesEl.onchange = function () {
      if (!tryFilesEl.checked) spaEl.checked = false;
    };
    $('m-save').onclick = function () {
      var name = $('p-name').value.trim();
      var bucket = $('p-bucket').value.trim();
      if (!name || !bucket) {
        $('p-error-msg').textContent = '名称与 Bucket 必填';
        return;
      }
      if ($('p-spa').checked && !$('p-try').checked) {
        $('p-error-msg').textContent = '启用 SPA 回退前必须先启用 try_files';
        return;
      }
      var spa = {
        enabled: $('p-spa').checked,
        exclude_prefixes: (p.spa && p.spa.exclude_prefixes) || [],
        exclude_extensions: (p.spa && p.spa.exclude_extensions) || []
      };
      var payload = {
        name: name,
        description: $('p-desc').value.trim(),
        index_doc: $('p-index').value.trim() || 'index.html',
        error_doc: $('p-error').value.trim(),
        try_files_enabled: $('p-try').checked,
        spa: spa,
        package_retention: $('p-retain').checked
      };
      var keepVersionsRaw = $('p-keep-versions').value.trim();
      var keepDaysRaw = $('p-keep-days').value.trim();
      if (existing) {
        // PATCH 语义：清空且原值存在时发送显式 null 表示清除并回退继承全局；未提供表示保持不变。
        if (keepVersionsRaw === '' && p.keep_versions != null) payload.keep_versions = null;
        if (keepDaysRaw === '' && p.keep_days != null) payload.keep_days = null;
      }
      if (keepVersionsRaw !== '') {
        var kv = parseInt(keepVersionsRaw, 10);
        if (isNaN(kv) || kv < 0) { $('p-error-msg').textContent = '版本保留数量必须为不小于 0 的整数'; return; }
        payload.keep_versions = kv;
      }
      if (keepDaysRaw !== '') {
        var kd = parseInt(keepDaysRaw, 10);
        if (isNaN(kd) || kd < 0) { $('p-error-msg').textContent = '版本保留天数必须为不小于 0 的整数'; return; }
        payload.keep_days = kd;
      }
      try {
        payload.response_headers = parseHeaders($('p-headers').value);
      } catch (err) {
        $('p-error-msg').textContent = err.message;
        return;
      }
      if (!existing) {
        payload.alias_id = $('p-alias').value;
        payload.bucket = bucket;
      }
      var req = existing ? api('PATCH', '/projects/' + existing.id, payload) : api('POST', '/projects', payload);
      run(req).then(function () {
        closeModal();
        showToast(existing ? '项目已更新' : '项目已创建');
        loadProjects();
      }).catch(function (err) {
        $('p-error-msg').textContent = err.message;
      });
    };
  }

  function openProjectDetail(projectId) {
    var project = state.projects.filter(function (p) { return p.id === projectId; })[0];
    if (!project) return;
    if (detailProjectId !== projectId) delete listState.versions;
    detailProjectId = projectId;
    routeTo('#/projects/' + projectId);
    showView('project-detail', { fromRoute: true, skipLoad: true });

    var nameEl = $('detail-project-name');
    if (nameEl) nameEl.textContent = '· ' + project.name;

    var bodyEl = $('detail-body');
    if (bodyEl) bodyEl.innerHTML = '<div class="empty">加载中…</div>';

    var updateVersionRows = function () {
      renderList({
        key: 'versions',
        tbody: 'tbody-versions',
        empty: 'empty-versions',
        pager: 'pager-versions',
        items: state.versions[projectId] || [],
        emptyText: '还没有版本，请先上传发布包',
        searchText: function (v) { return searchJoin(v.id, v.label, v.status); },
        row: function (v) {
          var isCurrent = v.id === project.current_version_id;
          var badge = isCurrent ? '<span class="badge ok">当前</span>' : '<span class="badge mute">' + esc(v.status) + '</span>';
          if (v.protected) badge += ' <span class="badge warn">保护</span>';
          var tr = document.createElement('tr');
          tr.innerHTML =
            '<td class="mono ellipsis" title="' + esc(v.id) + '">' + esc(shortId(v.id)) + '</td>' +
            '<td>' + badge + '</td>' +
            '<td>' + (v.file_count || 0) + '</td>' +
            '<td>' + bytes(v.total_bytes) + '</td>' +
            '<td class="mono ellipsis" title="' + esc(v.label || '') + '">' + esc(v.label || '—') + '</td>' +
            '<td>' + esc(new Date(v.created_at).toLocaleString()) + '</td>' +
            '<td class="right"><div class="row-actions">' +
            (isCurrent ? '' : '<button class="btn secondary small" data-act="pointer" data-id="' + esc(v.id) + '">设为当前</button>') +
            '<button class="btn secondary small" data-act="direct" data-id="' + esc(v.id) + '">直链</button>' +
            (project.package_retention && v.status === 'ready' ? '<button class="btn secondary small" data-act="package" data-id="' + esc(v.id) + '">原包</button>' : '') +
            '<button class="btn secondary small" data-act="protect" data-id="' + esc(v.id) + '" data-protected="' + (v.protected ? '0' : '1') + '">' + (v.protected ? '取消保护' : '保护') + '</button>' +
            (isCurrent ? '' : '<button class="btn danger small" data-act="vdelete" data-id="' + esc(v.id) + '">删除</button>') +
            '</div></td>';
          var pointerBtn = tr.querySelector('[data-act="pointer"]');
          if (pointerBtn) {
            pointerBtn.onclick = function () {
              run(api('POST', '/projects/' + projectId + '/pointer', { version_id: v.id })).then(function () {
                showToast('当前指针已切换');
                project.current_version_id = v.id;
                openProjectDetail(projectId);
                loadProjects();
              }).catch(function () {});
            };
          }
          tr.querySelector('[data-act="direct"]').onclick = function () {
            showPath('版本直链', versionDirectPath(projectId, v.id));
          };
          var packageBtn = tr.querySelector('[data-act="package"]');
          if (packageBtn) {
            packageBtn.onclick = function () { downloadPackage(projectId, v.id); };
          }
          var deleteBtn = tr.querySelector('[data-act="vdelete"]');
          if (deleteBtn) {
            deleteBtn.onclick = function () {
              if (!window.confirm('确认删除版本 ' + shortId(v.id) + '？删除后不可访问，对象将由 GC 异步清理。')) return;
              run(api('DELETE', '/projects/' + projectId + '/versions/' + v.id)).then(function () {
                showToast('版本已删除');
                openProjectDetail(projectId);
              }).catch(function () {});
            };
          }
          tr.querySelector('[data-act="protect"]').onclick = function () {
            var next = !v.protected;
            run(api('POST', '/projects/' + projectId + '/versions/' + v.id + '/protect', { protected: next }))
              .then(function () {
                showToast(next ? '版本已保护' : '已取消保护');
                openProjectDetail(projectId);
              }).catch(function () {});
          };
          return tr;
        }
      });
    };

    var render = function () {
      var body = $('detail-body');
      body.innerHTML =
        '<div class="card"><div class="detail-meta">' +
        '<div class="kv"><span>项目 ID</span><code>' + esc(project.id) + '</code></div>' +
        '<div class="kv"><span>Bucket</span><code>' + esc(project.bucket) + '</code></div>' +
        '<div class="kv"><span>当前版本</span><code>' + esc(shortId(project.current_version_id)) + '</code></div>' +
        '<div class="kv"><span>index / 404</span><code>' + esc(project.index_doc || 'index.html') + ' / ' + esc(project.error_doc || '—') + '</code></div>' +
        '<div class="kv"><span>try_files / SPA</span><code>' + (project.try_files_enabled ? '开启' : '关闭') + ' / ' +
        (project.spa && project.spa.enabled
          ? (project.try_files_enabled ? '开启' : '开启（未启用 try_files，不生效）')
          : '关闭') + '</code></div>' +
        '<div class="kv"><span>版本保留策略</span><code>' + esc(retentionText(project)) + '</code></div>' +
        '</div></div>' +
        '<div class="card publish-card">' +
        '<div class="publish-head">' +
        '<h3>发布新版本</h3>' +
        '<p>上传发布包（.zip / .tar / .tar.gz），系统会自动识别格式、解包、校验并生成 manifest；默认完成后自动切换当前指针，取消勾选则只上传不发布。</p>' +
        '</div>' +
        '<div class="publish-grid">' +
        '<label class="field"><span>发布包（.zip / .tar / .tar.gz）</span><input type="file" id="pkg-file" accept=".zip,.tar,.tar.gz,application/zip,application/x-tar,application/gzip"></label>' +
        '<label class="field"><span>版本标签（可选）</span><input type="text" id="pub-label" placeholder="如 git sha，同一项目内唯一"></label>' +
        '</div>' +
        '<div class="publish-foot">' +
        '<label class="check"><input type="checkbox" id="pub-auto" checked><span>完成后自动切换为当前版本</span></label>' +
        '<button class="btn" id="btn-publish">上传并发布</button>' +
        '<span class="publish-status" id="publish-status"></span>' +
        '</div>' +
        '</div>' +
        '<div class="card table-wrap" style="box-shadow:none">' +
        '<div class="list-bar">' +
        '<div class="search-box">' +
        '<svg viewBox="0 0 16 16" fill="none"><circle cx="7" cy="7" r="5" stroke="currentColor" stroke-width="1.6"/><path d="M11 11l3.2 3.2" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>' +
        '<input type="search" id="search-versions" class="search-input" placeholder="搜索版本 ID、标签、状态">' +
        '</div>' +
        '<select class="page-size" id="ps-versions">' +
        '<option value="10">10 条/页</option>' +
        '<option value="20">20 条/页</option>' +
        '<option value="50">50 条/页</option>' +
        '</select>' +
        '</div>' +
        '<table><thead><tr><th style="width:100px">版本</th><th style="width:110px">状态</th><th style="width:76px">文件数</th><th style="width:96px">总大小</th><th style="width:110px">标签</th><th style="width:150px">创建时间</th><th class="right">操作</th></tr></thead>' +
        '<tbody id="tbody-versions"></tbody></table>' +
        '<div class="empty" id="empty-versions">还没有版本，请先上传发布包</div>' +
        '<div class="pagination hidden" id="pager-versions"></div>' +
        '</div>';

      wireListControls('versions', updateVersionRows);
      updateVersionRows();

      $('btn-publish').onclick = function () {
        var file = $('pkg-file').files[0];
        if (!file) { showToast('请先选择发布包', true); return; }
        publish(project, file);
      };
    };

    run(api('GET', '/projects/' + projectId + '/versions')).then(function (versions) {
      state.versions[projectId] = versions || [];
      render();
    }).catch(function () {
      var body = $('detail-body');
      if (body) body.innerHTML = '<div class="empty">版本列表加载失败</div>';
    });
  }

  function publish(project, file) {
    var status = $('publish-status');
    var btn = $('btn-publish');
    btn.disabled = true;
    status.textContent = '创建发布任务…';

    var payload = { project_id: project.id, mode: 'auto' };
    var labelInput = $('pub-label');
    var label = labelInput ? labelInput.value.trim() : '';
    if (label) payload.label = label;
    var autoInput = $('pub-auto');
    var autoPublish = !autoInput || autoInput.checked;
    if (!autoPublish) payload.auto_publish = false;

    api('POST', '/deployments', payload)
      .then(function (dep) {
        var id = dep.deployment_id;
        status.textContent = '上传包 ' + bytes(file.size) + '…';
        return api('PUT', '/deployments/' + id + '/package', file, 'application/octet-stream').then(function () { return id; });
      })
      .then(function (id) {
        status.textContent = '解包并生成 manifest…';
        return api('POST', '/deployments/' + id + '/complete').then(function (result) {
          status.textContent = '完成：' + (result.status || 'ready') + (autoPublish ? '' : '（未切换当前版本，可在版本列表点「设为当前」上线）');
          showToast(autoPublish ? '发布成功' : '上传成功（未发布）');
          setTimeout(function () { openProjectDetail(project.id); }, 400);
          loadProjects();
          return result;
        });
      })
      .catch(function (err) {
        status.textContent = '';
        showToast(err.message, true);
      })
      .then(function () { btn.disabled = false; });
  }

  // ----- bindings -----

  function loadBindings() {
    return run(Promise.all([
      api('GET', '/bindings'),
      api('GET', '/projects'),
      api('GET', '/auth-policies')
    ]).then(function (res) {
      state.bindings = res[0] || [];
      state.projects = res[1] || [];
      state.authPolicies = res[2] || [];
      renderBindings();
    })).catch(function () {});
  }

  function authPolicyName(id) {
    if (!id) return '';
    var found = state.authPolicies.filter(function (p) { return p.id === id; })[0];
    return found ? found.name : shortId(id);
  }

  function projectName(id) {
    var found = state.projects.filter(function (p) { return p.id === id; })[0];
    return found ? found.name : shortId(id);
  }

  function renderBindings() {
    renderList({
      key: 'bindings',
      tbody: 'tbody-bindings',
      empty: 'empty-bindings',
      pager: 'pager-bindings',
      items: state.bindings,
      emptyText: '暂无绑定',
      searchText: function (b) {
        return searchJoin(b.host, b.path_prefix, projectName(b.project_id), authPolicyName(b.auth_policy_id), b.enabled ? '启用' : '停用');
      },
      row: function (b) {
        var tr = document.createElement('tr');
        tr.innerHTML =
          '<td class="ellipsis" title="' + esc(b.host) + '"><strong>' + esc(b.host) + '</strong></td>' +
          '<td class="mono ellipsis" title="' + esc(b.path_prefix || '/') + '">' + esc(b.path_prefix || '/') + '</td>' +
          '<td>' + esc(projectName(b.project_id)) + '</td>' +
          '<td>' + (b.auth_policy_id ? '<span class="badge ok">Basic · ' + esc(authPolicyName(b.auth_policy_id)) + '</span>' : '<span class="badge mute">公开</span>') + '</td>' +
          '<td class="mono">' + esc(b.pinned_version_id ? shortId(b.pinned_version_id) : '跟随指针') + '</td>' +
          '<td>' + (b.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge mute">停用</span>') + '</td>' +
          '<td class="right"><div class="row-actions">' +
          '<button class="btn secondary small" data-act="edit">编辑</button>' +
          '<button class="btn danger small" data-act="delete">删除</button>' +
          '</div></td>';
        tr.querySelector('[data-act="edit"]').onclick = function () { openBindingModal(b); };
        tr.querySelector('[data-act="delete"]').onclick = function () {
          if (!confirm('删除绑定 ' + b.host + (b.path_prefix || '/') + ' ?')) return;
          run(api('DELETE', '/bindings/' + b.id)).then(function () {
            showToast('绑定已删除');
            loadBindings();
          }).catch(function () {});
        };
        return tr;
      }
    });
  }

  function openBindingModal(existing) {
    var b = existing || {};
    if (!state.projects.length) {
      showToast('请先创建项目', true);
      showView('projects');
      return;
    }
    var options = state.projects.map(function (p) {
      return '<option value="' + esc(p.id) + '"' + (b.project_id === p.id ? ' selected' : '') + '>' + esc(p.name) + ' (' + esc(p.bucket) + ')</option>';
    }).join('');
    var authOptions = '<option value="">无（公开访问）</option>' + state.authPolicies.map(function (p) {
      return '<option value="' + esc(p.id) + '"' + (b.auth_policy_id === p.id ? ' selected' : '') + '>' +
        esc(p.name) + ' · Basic · ' + p.user_count + ' 个用户</option>';
    }).join('');
    if (b.auth_policy_id && !state.authPolicies.filter(function (p) { return p.id === b.auth_policy_id; }).length) {
      authOptions += '<option value="' + esc(b.auth_policy_id) + '" selected>' + esc(shortId(b.auth_policy_id)) + '（已删除或不可用）</option>';
    }
    openModal({
      title: existing ? '编辑绑定' : '新建绑定',
      body:
        '<div class="form">' +
        '<div class="grid2">' +
        '<label>Host<input id="b-host" value="' + esc(b.host || '') + '" placeholder="www.example.com"></label>' +
        '<label>路径前缀<input id="b-prefix" value="' + esc(b.path_prefix || '/') + '" placeholder="/"></label>' +
        '</div>' +
        '<label>项目<select id="b-project">' + options + '</select></label>' +
        '<label>访问鉴权<select id="b-auth">' + authOptions + '</select></label>' +
        '<p class="hint">默认「无（公开访问）」；选择 Basic 策略后，该 Host + 路径前缀下的所有请求都需要用户名密码。策略与用户请在「访问鉴权」页维护。</p>' +
        '<div class="grid2">' +
        '<label>固定版本<select id="b-pinned"><option value="">跟随项目指针</option></select></label>' +
        '<label class="inline" style="align-self:end"><input type="checkbox" id="b-enabled" ' + (b.enabled !== false ? 'checked' : '') + '> 启用绑定</label>' +
        '</div>' +
        '<label>响应头（每行 Name: Value，覆盖项目同名头）<textarea id="b-headers" rows="3" placeholder="X-Robots-Tag: noindex">' + esc(headersText(b.response_headers)) + '</textarea></label>' +
        '<div class="form-error" id="b-error"></div>' +
        '</div>',
      footer:
        '<button class="btn ghost" id="m-cancel">取消</button>' +
        '<button class="btn" id="m-save">保存</button>'
    });

    var pinned = $('b-pinned');
    var fillVersions = function (projectId) {
      pinned.innerHTML = '<option value="">跟随项目指针</option>';
      if (!projectId) return;
      var cached = state.versions[projectId];
      if (cached) {
        cached.forEach(function (v) {
          pinned.innerHTML += '<option value="' + esc(v.id) + '"' + (b.pinned_version_id === v.id ? ' selected' : '') + '>' +
            esc(shortId(v.id)) + ' · ' + esc(v.status) + '</option>';
        });
        return;
      }
      api('GET', '/projects/' + projectId + '/versions').then(function (versions) {
        state.versions[projectId] = versions || [];
        fillVersions(projectId);
      }).catch(function () {});
    };
    $('b-project').onchange = function () { fillVersions($('b-project').value); };
    fillVersions($('b-project').value);

    $('m-cancel').onclick = closeModal;
    $('m-save').onclick = function () {
      var host = $('b-host').value.trim();
      if (!host) { $('b-error').textContent = 'Host 必填'; return; }
      var payload = {
        host: host,
        path_prefix: $('b-prefix').value.trim() || '/',
        project_id: $('b-project').value,
        pinned_version_id: $('b-pinned').value,
        auth_policy_id: $('b-auth').value.trim(),
        enabled: $('b-enabled').checked
      };
      try {
        payload.response_headers = parseHeaders($('b-headers').value);
      } catch (err) {
        $('b-error').textContent = err.message;
        return;
      }
      var req = existing ? api('PATCH', '/bindings/' + existing.id, payload) : api('POST', '/bindings', payload);
      run(req).then(function () {
        closeModal();
        showToast(existing ? '绑定已更新' : '绑定已创建');
        loadBindings();
      }).catch(function (err) {
        $('b-error').textContent = err.message;
      });
    };
  }

  // ----- auth policies -----

  function loadAuthPolicies() {
    return run(api('GET', '/auth-policies').then(function (items) {
      state.authPolicies = items || [];
      renderAuthPolicies();
    })).catch(function () {});
  }

  function renderAuthPolicies() {
    renderList({
      key: 'auth',
      tbody: 'tbody-auth',
      empty: 'empty-auth',
      pager: 'pager-auth',
      items: state.authPolicies,
      emptyText: '暂无鉴权策略',
      searchText: function (p) { return searchJoin(p.name, p.id); },
      row: function (p) {
        var tr = document.createElement('tr');
        tr.innerHTML =
          '<td><strong>' + esc(p.name) + '</strong><div class="mono">' + esc(shortId(p.id)) + '</div></td>' +
          '<td><span class="badge ok">Basic</span></td>' +
          '<td>' + esc(p.user_count) + '</td>' +
          '<td class="mono">v' + esc(p.secret_version) + '</td>' +
          '<td>' + esc(new Date(p.updated_at).toLocaleString()) + '</td>' +
          '<td class="right"><div class="row-actions">' +
          '<button class="btn secondary small" data-act="users">用户</button>' +
          '<button class="btn secondary small" data-act="edit">改名</button>' +
          '<button class="btn danger small" data-act="delete">删除</button>' +
          '</div></td>';
        tr.querySelector('[data-act="users"]').onclick = function () { openAuthUsersModal(p); };
        tr.querySelector('[data-act="edit"]').onclick = function () { openAuthPolicyModal(p); };
        tr.querySelector('[data-act="delete"]').onclick = function () {
          if (!confirm('删除鉴权策略「' + p.name + '」？仍被绑定引用时无法删除。')) return;
          run(api('DELETE', '/auth-policies/' + p.id)).then(function () {
            showToast('鉴权策略已删除');
            loadAuthPolicies();
          }).catch(function () {});
        };
        return tr;
      }
    });
  }

  function openAuthPolicyModal(existing) {
    openModal({
      title: existing ? '重命名鉴权策略' : '新建鉴权策略',
      body:
        '<div class="form">' +
        '<label>名称（同时作为 Basic realm）<input id="a-name" value="' + esc(existing ? existing.name : '') + '" placeholder="例如 internal-docs"></label>' +
        '<p class="hint">策略固定使用 HTTP Basic Auth；创建后在「用户」中维护用户名与密码。</p>' +
        '<div class="form-error" id="a-error"></div>' +
        '</div>',
      footer:
        '<button class="btn ghost" id="m-cancel">取消</button>' +
        '<button class="btn" id="m-save">保存</button>'
    });
    $('m-cancel').onclick = closeModal;
    $('m-save').onclick = function () {
      var name = $('a-name').value.trim();
      if (!name) { $('a-error').textContent = '名称必填'; return; }
      var req = existing
        ? api('PATCH', '/auth-policies/' + existing.id, { name: name })
        : api('POST', '/auth-policies', { name: name, mode: 'basic' });
      run(req).then(function () {
        closeModal();
        showToast(existing ? '策略已更新' : '策略已创建，请继续添加用户');
        loadAuthPolicies();
      }).catch(function (err) { $('a-error').textContent = err.message; });
    };
  }

  function openAuthUsersModal(policy) {
    run(api('GET', '/auth-policies/' + policy.id)).then(function (detail) {
      var users = detail.users || [];
      var rows = users.map(function (u) {
        return '<div class="kv"><span class="mono">' + esc(u.username) + '</span>' +
          '<span><button class="btn danger small" data-user="' + esc(u.username) + '">删除</button></span></div>';
      }).join('') || '<p class="hint">还没有用户，添加后该策略才会生效。</p>';
      openModal({
        title: '用户 · ' + policy.name,
        body:
          '<div class="form">' +
          '<div class="card" style="padding:12px">' + rows + '</div>' +
          '<div class="grid2">' +
          '<label>用户名<input id="u-name" placeholder="alice"></label>' +
          '<label>密码<input id="u-pass" type="password" placeholder="设置或替换密码"></label>' +
          '</div>' +
          '<p class="hint">用户名与密码用于浏览器弹出的 Basic Auth 对话框；保存后会立即推送到数据面。</p>' +
          '<div class="form-error" id="u-error"></div>' +
          '</div>',
        footer:
          '<button class="btn ghost" id="m-cancel">关闭</button>' +
          '<button class="btn" id="m-save">添加 / 更新用户</button>'
      });
      $('m-cancel').onclick = closeModal;
      var buttons = document.querySelectorAll('#modal-body [data-user]');
      for (var i = 0; i < buttons.length; i++) {
        buttons[i].onclick = function () {
          var username = this.getAttribute('data-user');
          if (!confirm('删除用户 ' + username + ' ?')) return;
          run(api('DELETE', '/auth-policies/' + policy.id + '/users/' + encodeURIComponent(username))).then(function () {
            showToast('用户已删除');
            openAuthUsersModal(policy);
          }).catch(function () {});
        };
      }
      $('m-save').onclick = function () {
        var username = $('u-name').value.trim();
        var password = $('u-pass').value;
        if (!username || !password) { $('u-error').textContent = '用户名与密码必填'; return; }
        run(api('PUT', '/auth-policies/' + policy.id + '/users/' + encodeURIComponent(username), { password: password })).then(function () {
          showToast('用户已保存');
          openAuthUsersModal(policy);
        }).catch(function (err) { $('u-error').textContent = err.message; });
      };
    }).catch(function () {});
  }

  // ----- deployments -----

  function badgeClass(status) {
    if (status === 'ready') return 'ok';
    if (status === 'failed') return 'err';
    if (status === 'canceled') return 'mute';
    return 'warn';
  }

  function loadDeployments() {
    return run(api('GET', '/deployments?limit=200')).then(function (list) {
      state.deploymentItems = list || [];
      renderDeployments();
    }).catch(function () {});
  }
  function renderDeployments() {
    renderList({
      key: 'deployments',
      tbody: 'tbody-deployments',
      empty: 'empty-deployments',
      pager: 'pager-deployments',
      items: state.deploymentItems || [],
      emptyText: '还没有发布记录，请到项目详情中上传发布包',
      searchText: function (item) {
        return searchJoin(item.project_name, item.deployment_id, item.status, item.version_id);
      },
      row: function (item) {
        var status = item.status || 'unknown';
        var tr = document.createElement('tr');
        tr.innerHTML =
          '<td>' + esc(new Date(item.created_at).toLocaleString()) + '</td>' +
          '<td class="ellipsis" title="' + esc(item.project_name) + '">' + esc(item.project_name) + '</td>' +
          '<td class="mono ellipsis" title="' + esc(item.deployment_id) + '">' + esc(shortId(item.deployment_id)) + '</td>' +
          '<td><span class="badge ' + badgeClass(status) + '">' + esc(status) + '</span></td>' +
          '<td class="mono">' + esc(shortId(item.version_id)) + '</td>' +
          '<td>' + (item.file_count == null ? '—' : item.file_count) + '</td>' +
          '<td class="right"><div class="row-actions">' +
          '<button class="btn secondary small" data-act="view">查看</button>' +
          (status === 'ready' || status === 'failed' || status === 'canceled' ? '' :
            '<button class="btn danger small" data-act="cancel">取消</button>') +
          '</div></td>';
        tr.querySelector('[data-act="view"]').onclick = function () {
          openModal({
            title: '发布详情',
            body: '<pre class="mono" style="white-space:pre-wrap">' + esc(JSON.stringify(item, null, 2)) + '</pre>',
            footer: '<button class="btn ghost" id="m-close">关闭</button>'
          });
          $('m-close').onclick = closeModal;
        };
        var cancelBtn = tr.querySelector('[data-act="cancel"]');
        if (cancelBtn) {
          cancelBtn.onclick = function () {
            run(api('POST', '/deployments/' + item.deployment_id + '/cancel')).then(function () {
              showToast('已取消发布');
              loadDeployments();
            }).catch(function () {});
          };
        }
        return tr;
      }
    });
  }

  // ----- monitoring -----

  function loadMonitoring() {
    return run(Promise.all([api('GET', '/metrics'), api('GET', '/logs?limit=200')]).then(function (res) {
      var m = res[0] || {};
      state.metrics = m;
      state.logs = res[1] || [];
      $('m-uptime').textContent = duration(m.uptime_seconds);
      $('m-total').textContent = m.total_requests || 0;
      $('m-4xx').textContent = m.status_4xx || 0;
      $('m-5xx').textContent = m.status_5xx || 0;
      renderLogs();
    })).catch(function () {});
  }

  function renderLogs() {
    renderList({
      key: 'logs',
      tbody: 'tbody-logs',
      empty: 'empty-logs',
      pager: 'pager-logs',
      items: state.logs || [],
      emptyText: '暂无请求记录',
      searchText: function (entry) {
        return searchJoin(entry.method, entry.path, entry.status, entry.request_id);
      },
      row: function (entry) {
        var cls = entry.status >= 500 ? 'err' : entry.status >= 400 ? 'warn' : 'ok';
        var tr = document.createElement('tr');
        tr.innerHTML =
          '<td class="mono">' + esc(new Date(entry.time).toLocaleTimeString()) + '</td>' +
          '<td>' + esc(entry.method) + '</td>' +
          '<td class="mono ellipsis" title="' + esc(entry.path) + '">' + esc(entry.path) + '</td>' +
          '<td><span class="badge ' + cls + '">' + entry.status + '</span></td>' +
          '<td>' + entry.duration_ms + ' ms</td>' +
          '<td>' + bytes(entry.bytes) + '</td>' +
          '<td class="mono ellipsis" title="' + esc(entry.request_id) + '">' + esc(shortId(entry.request_id)) + '</td>';
        return tr;
      }
    });
  }

  // ----- settings -----

  function openSettings() {
    openModal({
      title: '连接设置',
      body:
        '<div class="form">' +
        '<div class="section-note">管理 API 与控制台同源部署（' + esc(location.origin) + '）。' +
        '请填写控制面配置中的 <code>server.admin_token</code>，Token 仅保存在当前浏览器。</div>' +
        '<label>Admin Token<input id="s-token" type="password" value="' + esc(state.token) + '" placeholder="Bearer Token"></label>' +
        '<div class="kv"><span>健康检查</span><code id="s-health">未检测</code></div>' +
        '<div class="form-error" id="s-error"></div>' +
        '</div>',
      footer:
        '<button class="btn ghost" id="m-cancel">取消</button>' +
        '<button class="btn" id="m-save">保存并连接</button>'
    });
    $('m-cancel').onclick = closeModal;
    $('m-save').onclick = function () {
      var token = $('s-token').value.trim();
      state.token = token;
      localStorage.setItem(TOKEN_KEY, token);
      checkHealth().then(function (ok) {
        $('s-health').textContent = ok ? '正常' : '不可达';
        if (!ok) return;
        return api('GET', '/projects').then(function () {
          closeModal();
          showToast('已连接控制面');
          showView('overview');
        }).catch(function (err) {
          $('s-error').textContent = err.message;
        });
      });
    };
  }

  // ----- shares / direct links ----------

  function versionDirectPath(projectId, versionId) {
    return '/__pageforge/v/' + encodeURIComponent(projectId) + '/' + encodeURIComponent(versionId) + '/';
  }

  function sharePath(token) {
    return '/__pageforge/s/' + encodeURIComponent(token) + '/';
  }

  function showPath(title, path) {
    openModal({
      title: title,
      body: '<label>路径<input id="path-value" readonly value="' + esc(path) + '"></label>' +
        '<p class="hint">复制后拼接到数据面域名之后即可访问；分享链接可直接发给外部访客。</p>',
      footer: '<button class="btn ghost" id="m-cancel">关闭</button><button class="btn" id="m-copy">复制</button>'
    });
    $('m-cancel').onclick = closeModal;
    $('m-copy').onclick = function () { copyText($('path-value').value); };
    $('path-value').onclick = function () { this.select(); };
  }

  function downloadPackage(projectId, versionId) {
    return run(fetch('/api/v1/projects/' + encodeURIComponent(projectId) +
      '/versions/' + encodeURIComponent(versionId) + '/package', {
      headers: { Authorization: 'Bearer ' + state.token }
    }).then(function (res) {
      if (!res.ok) throw new Error('下载失败：HTTP ' + res.status);
      var cd = res.headers.get('Content-Disposition') || '';
      var m = cd.match(/filename="?([^";]+)"?/);
      var filename = m ? m[1] : projectId + '-' + versionId + '.zip';
      return res.blob().then(function (blob) { return { blob: blob, filename: filename }; });
    }).then(function (result) {
      var link = document.createElement('a');
      link.href = URL.createObjectURL(result.blob);
      link.download = result.filename;
      document.body.appendChild(link);
      link.click();
      document.body.removeChild(link);
      setTimeout(function () { URL.revokeObjectURL(link.href); }, 2000);
      showToast('开始下载原始发布包');
    }));
  }

  function loadShares() {
    return run(Promise.all([api('GET', '/share-links'), api('GET', '/projects')]).then(function (res) {
      state.shares = res[0] || [];
      state.projects = res[1] || [];
      renderShares();
    })).catch(function () {});
  }

  function renderShares() {
    renderList({
      key: 'shares',
      tbody: 'tbody-shares',
      empty: 'empty-shares',
      pager: 'pager-shares',
      items: state.shares,
      emptyText: '暂无分享链接',
      searchText: function (s) {
        return searchJoin(projectName(s.project_id), s.token, s.version_id, s.status);
      },
      row: function (s) {
        var statusBadge = s.status === 'active' ? '<span class="badge ok">生效</span>' :
          (s.status === 'expired' ? '<span class="badge warn">已过期</span>' : '<span class="badge mute">已停用</span>');
        var tr = document.createElement('tr');
        tr.innerHTML =
          '<td class="ellipsis" title="' + esc(projectName(s.project_id)) + '">' + esc(projectName(s.project_id)) + '</td>' +
          '<td class="mono">' + esc(s.version_id ? shortId(s.version_id) : '跟随指针') + '</td>' +
          '<td class="mono ellipsis" title="' + esc(s.token) + '">' + esc(s.token) + '</td>' +
          '<td>' + (s.password_set ? '<span class="badge mute">密码</span>' : '—') + '</td>' +
          '<td>' + esc(s.expires_at ? new Date(s.expires_at).toLocaleString() : '—') + '</td>' +
          '<td>' + (s.views || 0) + (s.max_views ? ' / ' + s.max_views : '') + '</td>' +
          '<td>' + statusBadge + '</td>' +
          '<td class="right"><div class="row-actions">' +
          '<button class="btn secondary small" data-act="copy">复制链接</button>' +
          '<button class="btn secondary small" data-act="edit">编辑</button>' +
          '<button class="btn ghost small" data-act="delete">删除</button>' +
          '</div></td>';
        tr.querySelector('[data-act="copy"]').onclick = function () { showPath('分享链接', sharePath(s.token)); };
        tr.querySelector('[data-act="edit"]').onclick = function () { openShareModal(s); };
        tr.querySelector('[data-act="delete"]').onclick = function () {
          if (!confirm('确定删除分享链接 ' + s.token + '？删除后将立即失效。')) return;
          run(api('DELETE', '/share-links/' + s.id)).then(function () {
            showToast('已删除分享链接');
            loadShares();
          }).catch(function () {});
        };
        return tr;
      }
    });
  }

  function openShareModal(existing) {
    var share = existing || { max_views: 0, cache: {}, enabled: true };
    var options = state.projects.map(function (p) {
      return '<option value="' + esc(p.id) + '"' + (share.project_id === p.id ? ' selected' : '') + '>' + esc(p.name) + '</option>';
    }).join('');
    var expiresValue = share.expires_at ? new Date(share.expires_at).toISOString().slice(0, 16) : '';

    openModal({
      title: existing ? '编辑分享链接' : '新建分享链接',
      body:
        '<label>项目<select id="s-project"' + (existing ? ' disabled' : '') + '>' + options + '</select></label>' +
        '<label>版本<select id="s-version"><option value="">跟随项目指针</option></select></label>' +
        '<div class="grid2">' +
        '<label>到期时间（留空表示不限期）<input type="datetime-local" id="s-expires" value="' + esc(expiresValue) + '"></label>' +
        '<label>最大访问次数（0 表示不限）<input type="number" id="s-max" min="0" value="' + (share.max_views || 0) + '"></label>' +
        '</div>' +
        '<label>访问密码（留空表示不需要）<input id="s-password" placeholder="' +
        (existing && share.password_set ? '已设置，留空不修改' : '无密码') + '"></label>' +
        (existing && share.password_set ? '<label class="inline"><input type="checkbox" id="s-clear-password"> 清除密码保护（保存后无需密码）</label>' : '') +
        '<label>HTML 缓存策略<input id="s-cache" value="' + esc((share.cache && share.cache.html) || 'no-cache') + '"></label>' +
        '<label class="inline"><input type="checkbox" id="s-enabled" ' + ((existing ? share.enabled : true) ? 'checked' : '') + '> 启用</label>' +
        '<div class="form-error" id="s-error"></div>',
      footer: '<button class="btn ghost" id="m-cancel">取消</button><button class="btn" id="m-save">保存</button>'
    });

    var projectSelect = $('s-project');
    var versionSelect = $('s-version');

    var loadVersions = function (projectId, selected) {
      versionSelect.innerHTML = '<option value="">跟随项目指针</option>';
      if (!projectId) return;
      api('GET', '/projects/' + projectId + '/versions').then(function (list) {
        (list || []).filter(function (v) { return v.status === 'ready'; }).forEach(function (v) {
          var option = document.createElement('option');
          option.value = v.id;
          option.textContent = shortId(v.id) + (v.label ? ' · ' + v.label : '');
          if (v.id === selected) option.selected = true;
          versionSelect.appendChild(option);
        });
      }).catch(function () {});
    };
    loadVersions(existing ? share.project_id : (projectSelect.value || (state.projects[0] && state.projects[0].id)), share.version_id);
    projectSelect.onchange = function () { loadVersions(projectSelect.value, ''); };

    var clearBox = $('s-clear-password');
    if (clearBox) {
      clearBox.onchange = function () {
        $('s-password').disabled = clearBox.checked;
        if (clearBox.checked) $('s-password').value = '';
      };
    }

    $('m-cancel').onclick = closeModal;
    $('m-save').onclick = function () {
      var projectId = projectSelect.value;
      if (!projectId) { $('s-error').textContent = '请选择项目'; return; }
      var expiresRaw = $('s-expires').value;
      var expires = expiresRaw ? new Date(expiresRaw).toISOString() : null;
      var maxViews = parseInt($('s-max').value || '0', 10);
      if (isNaN(maxViews) || maxViews < 0) { $('s-error').textContent = '最大访问次数必须是非负整数'; return; }
      var cache = { html: $('s-cache').value.trim() || 'no-cache' };
      var password = $('s-password').value;
      var clearPassword = $('s-clear-password') ? $('s-clear-password').checked : false;

      var payload, req;
      if (existing) {
        payload = {
          version_id: versionSelect.value,
          max_views: maxViews,
          cache: cache,
          enabled: $('s-enabled').checked
        };
        if (clearPassword) payload.password = '';
        else if (password) payload.password = password;
        if (expires) payload.expires_at = expires; else payload.clear_expiry = true;
        req = api('PATCH', '/share-links/' + existing.id, payload);
      } else {
        payload = {
          project_id: projectId,
          version_id: versionSelect.value,
          max_views: maxViews,
          cache: cache
        };
        if (expires) payload.expires_at = expires;
        if (password) payload.password = password;
        req = api('POST', '/share-links', payload);
      }
      run(req).then(function (created) {
        closeModal();
        showToast(existing ? '分享链接已更新' : '分享链接已创建');
        loadShares();
        if (!existing && created && created.token) showPath('分享链接', sharePath(created.token));
      }).catch(function (err) {
        $('s-error').textContent = err.message;
      });
    };
  }

  // ----- boot -----

  function boot() {
    state.token = localStorage.getItem(TOKEN_KEY) || '';
    var tabs = document.querySelectorAll('.tab');
    for (var i = 0; i < tabs.length; i++) {
      tabs[i].onclick = function () { showView(this.dataset.view); };
    }
    $('modal-close').onclick = closeModal;
    $('modal-backdrop').onclick = function (e) { if (e.target === this) closeModal(); };
    $('btn-settings').onclick = openSettings;
    $('btn-new-alias').onclick = function () { openAliasModal(null); };
    $('btn-new-project').onclick = function () { openProjectModal(null); };
    $('btn-new-binding').onclick = function () { openBindingModal(null); };
    $('btn-new-auth').onclick = function () { openAuthPolicyModal(null); };
    $('btn-new-share').onclick = function () { openShareModal(null); };
    $('btn-refresh-overview').onclick = loadOverview;
    $('btn-refresh-deployments').onclick = loadDeployments;
    $('btn-refresh-monitoring').onclick = loadMonitoring;
    $('btn-detail-back').onclick = function () {
      detailProjectId = null;
      // 直接切换视图（showView 会同步 hash），避免 routeTo 先置 appliedHash
      // 导致 hashchange 触发 applyRoute 时被 `location.hash === appliedHash` 提前 return
      showView('projects');
    };
    wireListControls('aliases', renderAliases);
    wireListControls('projects', renderProjects);
    wireListControls('bindings', renderBindings);
    wireListControls('auth', renderAuthPolicies);
    wireListControls('shares', renderShares);
    wireListControls('deployments', renderDeployments);
    wireListControls('logs', renderLogs);
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') closeModal(); });
    window.addEventListener('hashchange', applyRoute);

    checkHealth();
    applyRoute();
    if (!state.token) openSettings();
  }

  boot();
})();
