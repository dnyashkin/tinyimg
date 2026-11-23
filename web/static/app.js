// tinyimg — drag&drop, сжатие по одному запросу на файл (до 20),
// скачивание по одному или zip-архивом.
(function () {
  'use strict';

  var MAX_FILES = 20;
  var PARALLEL = 3;
  var WEBP_OK = true; // выясняется из /api/features (сборка без cgo без webp-кодека)

  function acceptedRe() {
    return WEBP_OK ? /\.(png|jpe?g|webp)$/i : /\.(png|jpe?g)$/i;
  }

  var dropzone = document.getElementById('dropzone');
  var fileInput = document.getElementById('file-input');
  var fileList = document.getElementById('file-list');
  var toolFooter = document.getElementById('tool-footer');
  var totalEl = document.getElementById('total');
  var downloadAllBtn = document.getElementById('download-all');

  var done = []; // {id, name} — успешно сжатые, для zip
  var queue = [];
  var active = 0;

  function fmtBytes(n) {
    if (n >= 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' МБ';
    return Math.max(1, Math.round(n / 1024)) + ' КБ';
  }

  function plural(n, one, few, many) {
    var m10 = n % 10, m100 = n % 100;
    if (m10 === 1 && m100 !== 11) return one;
    if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
    return many;
  }

  function addFiles(files) {
    var accepted = 0;
    var re = acceptedRe();
    for (var i = 0; i < files.length; i++) {
      var f = files[i];
      if (re.test(f.name) === false) continue;
      if (done.length + queue.length + active >= MAX_FILES) break;
      accepted++;
      queue.push(f);
      addRow(f);
    }
    if (accepted === 0) {
      alert('Нужны ' + (WEBP_OK ? 'PNG, JPEG или WebP' : 'PNG или JPEG') +
        '. Уже добавлено максимум ' + MAX_FILES + ' файлов.');
      return;
    }
    pump();
  }

  function addRow(file) {
    fileList.hidden = false;
    toolFooter.hidden = false;
    var row = document.createElement('div');
    row.className = 'file-row';
    row.innerHTML =
      '<div class="spinner"></div>' +
      '<div class="file-name"></div>' +
      '<div class="file-size file-status">сжимаем…</div>';
    row.querySelector('.file-name').textContent = file.name;
    row.dataset.state = 'pending';
    fileList.appendChild(row);
    file._row = row;
    updateTotal();
  }

  function pump() {
    while (active < PARALLEL && queue.length > 0) {
      var file = queue.shift();
      active++;
      compressFile(file);
    }
  }

  // postWithRetry — при 429 ждём Retry-After и повторяем (файл не теряется).
  function postWithRetry(form, attempt) {
    return fetch('/api/compress', { method: 'POST', body: form })
      .then(function (resp) {
        if (resp.status === 429 && attempt < 3) {
          var wait = (parseInt(resp.headers.get('Retry-After') || '2', 10) || 2) * 1000;
          return new Promise(function (resolve) { setTimeout(resolve, wait); })
            .then(function () { return postWithRetry(form, attempt + 1); });
        }
        return resp.json();
      });
  }

  function compressFile(file) {
    var row = file._row;
    row.dataset.state = 'working';
    var form = new FormData();
    form.append('file', file, file.name);

    postWithRetry(form, 0)
      .then(function (data) {
        if (!data.ok) {
          renderError(row, data.error || 'ошибка сжатия');
          return;
        }
        renderDone(row, data.file);
        done.push({ id: data.file.id, name: data.file.name });
      })
      .catch(function () {
        renderError(row, 'сеть недоступна');
      })
      .then(function () {
        active--;
        updateTotal();
        pump();
      });
  }

  function renderDone(row, f) {
    row.dataset.state = 'done';
    row.dataset.before = f.before;
    row.dataset.after = f.after;
    var saved = f.before > 0 ? Math.round((1 - f.after / f.before) * 100) : 0;
    row.innerHTML =
      '<div class="file-name" title=""></div>' +
      '<div class="file-size file-before"></div>' +
      '<div class="file-size file-after"></div>' +
      '<div class="file-badge"></div>' +
      '<a class="file-dl" download>Скачать</a>';
    row.querySelector('.file-name').textContent = f.name;
    row.querySelector('.file-name').title = f.strategy || '';
    row.querySelector('.file-before').textContent = fmtBytes(f.before);
    row.querySelector('.file-after').textContent = '→ ' + fmtBytes(f.after);
    row.querySelector('.file-badge').textContent = saved > 0 ? '−' + saved + '%' : '0%';
    var link = row.querySelector('.file-dl');
    link.href = '/api/file/' + f.id;
    link.setAttribute('download', f.name);
  }

  function renderError(row, msg) {
    row.dataset.state = 'error';
    var name = row.querySelector('.file-name').textContent;
    row.innerHTML = '<div class="file-name"></div><div class="file-error"></div>';
    row.querySelector('.file-name').textContent = name;
    row.querySelector('.file-error').textContent = msg;
  }

  function updateTotal() {
    var totalBefore = 0, totalAfter = 0;
    var rows = fileList.querySelectorAll('.file-row[data-state="done"]');
    rows.forEach(function (row) {
      totalBefore += Number(row.dataset.before) || 0;
      totalAfter += Number(row.dataset.after) || 0;
    });
    var pending = fileList.querySelectorAll('.file-row[data-state="pending"], .file-row[data-state="working"]').length;
    if (rows.length === 0) {
      totalEl.textContent = pending > 0
        ? 'Обрабатываем ' + pending + ' ' + plural(pending, 'файл', 'файла', 'файлов') + '…'
        : '';
    } else {
      var saved = totalBefore > 0 ? Math.round((1 - totalAfter / totalBefore) * 100) : 0;
      totalEl.textContent = 'Готово: ' + rows.length + ' ' +
        plural(rows.length, 'файл', 'файла', 'файлов') + ', −' + saved + '% размера' +
        (pending > 0 ? ', ещё ' + pending + ' в работе' : '');
    }
    downloadAllBtn.disabled = done.length === 0;
  }

  // Выбор файлов
  dropzone.addEventListener('click', function () { fileInput.click(); });
  fileInput.addEventListener('change', function () {
    addFiles(fileInput.files);
    fileInput.value = '';
  });

  // Drag & drop
  ['dragenter', 'dragover'].forEach(function (ev) {
    dropzone.addEventListener(ev, function (e) {
      e.preventDefault();
      dropzone.classList.add('dragover');
    });
  });
  ['dragleave', 'drop'].forEach(function (ev) {
    dropzone.addEventListener(ev, function (e) {
      e.preventDefault();
      dropzone.classList.remove('dragover');
    });
  });
  dropzone.addEventListener('drop', function (e) {
    addFiles(e.dataTransfer.files);
  });

  // Скачать всё
  downloadAllBtn.addEventListener('click', function () {
    if (done.length === 0) return;
    var ids = done.map(function (d) { return d.id; }).join(',');
    window.location.href = '/api/zip?ids=' + ids;
  });
})();

// Слайдер «до/после» в секции качества.
(function () {
  'use strict';

  var demos = [
    {
      before: '/demo/photo1-before.jpg', after: '/demo/photo1-after.jpg',
      left: 'Оригинал · 119 КБ', right: 'Сжато · 43 КБ (−64%)',
      credit: 'Фото: Dmytro Koplyk / Unsplash'
    },
    {
      before: '/demo/photo2-before.jpg', after: '/demo/photo2-after.jpg',
      left: 'Оригинал · 236 КБ', right: 'Сжато · 86 КБ (−64%)',
      credit: 'Фото: Matsvei Liatkouski / Unsplash'
    },
    {
      before: '/demo/photo3-before.jpg', after: '/demo/photo3-after.jpg',
      left: 'Оригинал · 393 КБ', right: 'Сжато · 121 КБ (−69%)',
      credit: 'Фото: Azim Khan Ronnie / Unsplash'
    },
    {
      before: '/demo/photo4-before.jpg', after: '/demo/photo4-after.jpg',
      left: 'Оригинал · 139 КБ', right: 'Сжато · 46 КБ (−67%)',
      credit: 'Фото: Maxim Berg / Unsplash'
    },
    {
      before: '/demo/photo5-before.jpg', after: '/demo/photo5-after.jpg',
      left: 'Оригинал · 213 КБ', right: 'Сжато · 78 КБ (−64%)',
      credit: 'Фото: Sies Kranen / Unsplash'
    },
    {
      before: '/demo/photo6-before.jpg', after: '/demo/photo6-after.jpg',
      left: 'Оригинал · 372 КБ', right: 'Сжато · 149 КБ (−60%)',
      credit: 'Фото: Haci / Unsplash'
    },
    {
      before: '/demo/photo7-before.png', after: '/demo/photo7-after.png',
      left: 'Оригинал · 2.2 МБ', right: 'Сжато · 414 КБ (−81%)',
      credit: 'Иллюстрация с прозрачным фоном'
    }
  ];

  var compare = document.getElementById('compare');
  if (!compare) return;
  var overlay = document.getElementById('compare-overlay');
  var handle = document.getElementById('compare-handle');
  var beforeImg = document.getElementById('compare-before');
  var afterImg = document.getElementById('compare-after');
  var labelLeft = document.getElementById('label-left');
  var labelRight = document.getElementById('label-right');
  var credit = document.getElementById('compare-credit');
  var dlBefore = document.getElementById('dl-before');
  var dlAfter = document.getElementById('dl-after');

  function applyDemo(d) {
    beforeImg.src = d.before;
    afterImg.src = d.after;
    labelLeft.textContent = d.left;
    labelRight.textContent = d.right;
    credit.textContent = d.credit;
    dlBefore.href = d.before;
    dlAfter.href = d.after;
    setPos(50);
  }
  applyDemo(demos[0]);

  function setPos(pct) {
    pct = Math.max(0, Math.min(100, pct));
    overlay.style.clipPath = 'inset(0 0 0 ' + pct + '%)';
    handle.style.left = pct + '%';
  }
  setPos(50);

  var dragging = false;
  function onMove(e) {
    if (!dragging) return;
    var rect = compare.getBoundingClientRect();
    setPos((e.clientX - rect.left) / rect.width * 100);
  }
  compare.addEventListener('pointerdown', function (e) {
    dragging = true;
    compare.setPointerCapture(e.pointerId);
    onMove(e);
  });
  compare.addEventListener('pointermove', onMove);
  compare.addEventListener('pointerup', function () { dragging = false; });
  compare.addEventListener('pointercancel', function () { dragging = false; });

  // Переключение примеров: табы строятся из demos (единый источник правды)
  var tabsWrap = document.getElementById('compare-tabs');
  demos.forEach(function (d, i) {
    var tab = document.createElement('button');
    tab.className = 'compare-tab' + (i === 0 ? ' active' : '');
    tab.dataset.demo = String(i);
    tab.textContent = 'Пример ' + (i + 1) + ' · ' + d.right.match(/−\d+%/)[0];
    tabsWrap.appendChild(tab);
  });

  // Капабилити сборки: без cgo webp-кодека нет — прячем обещание WebP,
  // чтобы не предлагать пользователю формат, который сервер не примет.
  fetch('/api/features')
    .then(function (r) { return r.json(); })
    .then(function (f) {
      if (!f || f.webp !== false) return;
      WEBP_OK = false;
      fileInput.setAttribute('accept', '.png,.jpg,.jpeg');
      var hints = document.querySelectorAll('.webp-only');
      for (var i = 0; i < hints.length; i++) hints[i].style.display = 'none';
      var plain = document.querySelectorAll('.no-webp');
      for (var j = 0; j < plain.length; j++) plain[j].style.display = 'inline';
    })
    .catch(function () { /* сервис недоступен — оставляем как есть */ });

  var tabs = document.querySelectorAll('.compare-tab');
  tabs.forEach(function (tab) {
    tab.addEventListener('click', function () {
      tabs.forEach(function (t) { t.classList.remove('active'); });
      tab.classList.add('active');
      applyDemo(demos[Number(tab.dataset.demo)]);
    });
  });
})();
