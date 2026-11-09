(function () {
  var APP = 'site-alpha', VER = 'v1';
  function render() {
    document.getElementById('view').textContent =
      'app:      ' + APP + '\n' +
      'version:  ' + VER + '\n' +
      'path:     ' + location.pathname + '\n' +
      'hash:     ' + (location.hash || '(none)') + '\n' +
      'time:     ' + new Date().toISOString();
  }
  window.addEventListener('hashchange', render);
  render();
})();