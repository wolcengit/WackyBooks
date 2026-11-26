import { createApp, computed, nextTick, onMounted, ref, watch } from './vendor/vue.js';

createApp({
  setup() {
    const navigation = [['home','主页','⌂'],['favorites','收藏','☆'],['publishers','出版社','▤'],['authors','作者','♙']];
    const empty = () => ({ libraries: [], books: [], directories: {}, lists: {}, reading: {}, favoriteDates: {} });
    const catalog = ref(empty()), ready = ref(false), signed = ref(false), setup = ref(false);
    const busy = ref(false), notice = ref(''), query = ref(''), password = ref('');
    const currentPassword = ref(''), newPassword = ref(''), imageVersion = ref(0);
    const username = ref('Admin'), ldapEnabled = ref(false), oidcEnabled = ref(false), identity = ref(null);
    const admin = computed(() => identity.value?.admin === true);
    const ldapForm = ref(null), ldapMessage = ref(''), ldapError = ref('');
    const ldapTestUsername = ref(''), ldapTestPassword = ref('');
    const oidcForm = ref(null), oidcMessage = ref(''), oidcError = ref('');
    const users = ref([]), userForm = ref({}), accessForm = ref({});
    const account = ref(null), dialog = ref(null), dialogMode = ref(''), dialogError = ref('');
    const libraryForm = ref({}), listName = ref(''), selectedBook = ref(null);
    const directoryBrowser = ref(null), directoryBusy = ref(false), directoryError = ref('');
    const parts = ref([]);
    const expandedFolders = ref(new Set());
    const returnRoutes = new Map();
    const page = computed(() => parts.value[0] || 'home');
    const standalone = computed(() => ['search','read','admin','profile'].includes(page.value));
    const book = computed(() => catalog.value.books.find(b => String(b.ID) === parts.value[1]));
    const library = computed(() => catalog.value.libraries.find(l => String(l.ID) === parts.value[1]));
    const position = ref(0), progress = ref(null), pageCount = ref(null), bookText = ref(''), textReader = ref(null);
    const pdfFallback = ref(false);
    const preview = ref(null), previewBusy = ref(false), previewError = ref('');
    const formats = computed(() => [...new Set(catalog.value.books.map(item => item.Format))].sort());
    const previewUsesScroll = computed(() => ['text', 'markdown', 'document'].includes(preview.value?.type));
    // Pack the worksheet index and page index into the existing reading position.
    const pagesPerSheet = 100000;
    let previewController;
    const searchLibrary = ref(''), searchFormat = ref(''), searchOrder = ref('title');
    const searchBooks = ref([]), searchMore = ref(false), searchBusy = ref(false), searchError = ref('');
    let searchRequest = 0;
    const dateFormat = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' });
    const dateLabel = value => dateFormat.format(new Date(value));
    function durationLabel(ms) {
      const seconds = Math.floor(ms / 1000);
      if (seconds < 60) return seconds + ' 秒';
      const minutes = Math.floor(seconds / 60);
      if (minutes < 60) return minutes + ' 分 ' + seconds % 60 + ' 秒';
      return Math.floor(minutes / 60) + ' 小时 ' + minutes % 60 + ' 分';
    }
    const link = (...segments) => '#' + segments.map(encodeURIComponent).join('/');
    const bookURL = (item, action) => '/api/book/' + item.ID + '/' + action;
    let scrollTimer, readerGeneration = 0, activeReading = null;

    function readRoute() {
      const before = parts.value;
      try {
        parts.value = (location.hash.slice(1) || 'home').split('/').map(decodeURIComponent);
      } catch {
        parts.value = ['home'];
      }
      const current = link(...parts.value);
      const prior = link(...before);
      if (standalone.value && before.length && current !== prior) {
        if (returnRoutes.get(prior) !== current) returnRoutes.set(current, prior);
        else returnRoutes.delete(prior);
      }
      if (!standalone.value) returnRoutes.clear();
      if (page.value === 'search') query.value = parts.value[1] || '';
      if (signed.value && page.value === 'admin' && !admin.value) go('home');
      if (page.value === 'library') {
        for (let i = 3; i <= parts.value.length; i++) {
          expandedFolders.value.add(link(...parts.value.slice(0, i)));
        }
      }
      if (account.value) account.value.open = false;
      window.scrollTo(0, 0);
    }
    function go(...segments) {
      if (account.value) account.value.open = false;
      location.hash = link(...segments);
    }
    function back() { location.hash = returnRoutes.get(link(...parts.value)) || 'home'; }
    function search() {
      const value = query.value.trim();
      if (page.value === 'search' && (parts.value[1] || '') === value) void loadSearch();
      else go('search', value);
    }
    async function loadSearch(append = false) {
      const request = ++searchRequest;
      searchBusy.value = true;
      searchError.value = '';
      if (!append) {
        searchBooks.value = [];
        searchMore.value = false;
      }
      const params = new URLSearchParams({
        q: parts.value[1] || '', library: searchLibrary.value, format: searchFormat.value,
        sort: searchOrder.value, offset: append ? searchBooks.value.length : 0,
      });
      try {
        const result = await (await api('/api/search?' + params)).json();
        if (request !== searchRequest) return;
        searchBooks.value = append ? [...searchBooks.value, ...result.books] : result.books;
        searchMore.value = result.more;
      } catch (error) {
        if (request === searchRequest) searchError.value = error.message;
      } finally {
        if (request === searchRequest) searchBusy.value = false;
      }
    }
    watch([signed, page, () => parts.value[1], searchLibrary, searchFormat, searchOrder], () => {
      if (signed.value && page.value === 'search') void loadSearch();
      else ++searchRequest;
    });

    async function api(path, method = 'GET', body, keepalive = false, signal) {
      const response = await fetch(path, {
        method, keepalive, signal,
        headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (!response.ok) {
        const error = await response.json().catch(() => ({}));
        if (response.status === 401 && signed.value) {
          signed.value = false;
          identity.value = null;
          username.value = 'Admin';
          catalog.value = empty();
          dialog.value?.close();
          void loadSession().catch(() => {});
        }
        throw new Error(error.error || '请求失败');
      }
      return response;
    }
    async function loadCatalog(rescan = false) {
      const data = await (await api(rescan ? '/api/scan' : '/api/catalog', rescan ? 'POST' : 'GET')).json();
      catalog.value = { ...data, libraries: data.libraries || [] };
      if (!catalog.value.libraries.some(item => item.ID === searchLibrary.value)) searchLibrary.value = '';
      imageVersion.value++;
    }
    async function task(work, errorTarget = notice) {
      if (busy.value) return;
      busy.value = true;
      errorTarget.value = '';
      try { await work(); }
      catch (error) { errorTarget.value = error.message; }
      finally { busy.value = false; }
    }
    function login() {
      return task(async () => {
        const user = await (await api('/api/login', 'POST', { username: username.value, password: password.value })).json();
        password.value = '';
        setup.value = false;
        await loadCatalog();
        identity.value = { username: user.Username, admin: user.Admin, source: user.Source };
        signed.value = true;
        if (page.value === 'admin' && !admin.value) go('home');
      });
    }
    function logout() {
      return task(async () => {
        await api('/api/logout', 'POST');
        signed.value = false;
        identity.value = null;
        ldapForm.value = null;
        username.value = 'Admin';
        catalog.value = empty();
        returnRoutes.clear();
        go('home');
        await loadSession();
      });
    }
    function changePassword() {
      return task(async () => {
        await api('/api/password', 'POST', { current: currentPassword.value, password: newPassword.value });
        currentPassword.value = newPassword.value = '';
        notice.value = '密码已更新';
      });
    }
    function scan() {
      return task(async () => {
        await loadCatalog(true);
        notice.value = '扫描完成，共找到 ' + catalog.value.books.length + ' 本书';
      });
    }
    function scanLibrary(lib) {
      return task(async () => {
        await api('/api/library/scan', 'POST', { ID: lib.ID });
        await loadCatalog();
        const count = catalog.value.books.filter(item => String(item.Library) === String(lib.ID)).length;
        notice.value = '书库「' + lib.Name + '」重扫完成，共找到 ' + count + ' 本书';
      });
    }
    async function loadLDAP() {
      const settings = await (await api('/api/ldap')).json();
      ldapForm.value = {
        ...settings,
        UserFilter: settings.UserFilter || '(uid={username})',
        IDAttribute: settings.IDAttribute || 'entryUUID',
      };
    }
    async function loadOIDC() {
      const settings = await (await api('/api/oidc')).json();
      oidcForm.value = { ...settings };
    }
    function oidcAction(test) {
      return task(async () => {
        oidcMessage.value = '';
        await api(test ? '/api/oidc/test' : '/api/oidc', 'POST', oidcForm.value);
        if (!test) {
          await loadOIDC();
          await loadSession();
        }
        oidcMessage.value = test ? 'OIDC 发现文档和登录端点可用' : 'OIDC 配置已保存';
      }, oidcError);
    }
    function ldapAction(test) {
      return task(async () => {
        ldapMessage.value = '';
        const TestUsername = ldapTestUsername.value.trim(), TestPassword = ldapTestPassword.value;
        const testLogin = test && (TestUsername || TestPassword);
        await api(test ? '/api/ldap/test' : '/api/ldap', 'POST',
          testLogin ? { ...ldapForm.value, TestUsername, TestPassword } : ldapForm.value);
        if (!test) {
          ldapEnabled.value = ldapForm.value.Enabled;
          await loadLDAP();
        }
        ldapMessage.value = testLogin ? 'LDAP 用户搜索和密码验证通过' : test ? 'LDAP 连接和搜索范围验证通过' : 'LDAP 配置已保存';
      }, ldapError);
    }
    async function loadUsers() {
      users.value = await (await api('/api/users')).json();
    }
    function editUser(user) {
      userForm.value = user ? { ...user, Password: '' } : { Username: '', Password: '', Disabled: false, Source: 'local' };
      showDialog('user');
    }
    function editAccess(user) {
      accessForm.value = { ID: user.ID, Username: user.Username, LibraryIDs: [...(user.LibraryIDs || [])] };
      showDialog('access');
    }
    function saveUser() {
      return task(async () => {
        await api('/api/users', 'POST', userForm.value);
        await loadUsers();
        dialog.value.close();
        notice.value = '用户已保存';
      }, dialogError);
    }
    function saveAccess() {
      return task(async () => {
        await api('/api/users/access', 'POST', accessForm.value);
        await loadUsers();
        dialog.value.close();
        notice.value = '书库授权已保存';
      }, dialogError);
    }
    watch([page, admin, signed], ([route, isAdmin, isSigned]) => {
      if (route === 'admin' && isAdmin && isSigned) {
        ldapForm.value = null;
        ldapMessage.value = '';
        ldapError.value = '';
        ldapTestUsername.value = '';
        ldapTestPassword.value = '';
        void loadLDAP().catch(error => { ldapError.value = error.message; });
        oidcForm.value = null;
        oidcMessage.value = '';
        oidcError.value = '';
        void loadOIDC().catch(error => { oidcError.value = error.message; });
        void loadUsers().catch(error => { notice.value = error.message; });
      }
    });
    function showDialog(mode) {
      dialogMode.value = mode;
      dialogError.value = '';
      if (!dialog.value.open) dialog.value.showModal();
    }
    function editLibrary(lib) {
      libraryForm.value = lib ? { ...lib } : { Name: '', Directory: '' };
      directoryBrowser.value = null;
      directoryError.value = '';
      showDialog('library');
    }
    async function browseDirectory(path = libraryForm.value.Directory) {
      if (directoryBusy.value) return;
      directoryBusy.value = true;
      directoryError.value = '';
      try {
        const params = new URLSearchParams();
        if (path?.trim()) params.set('path', path.trim());
        const query = params.toString();
        directoryBrowser.value = await (await api('/api/directories' + (query ? '?' + query : ''))).json();
      } catch (error) {
        directoryError.value = error.message;
      } finally {
        directoryBusy.value = false;
      }
    }
    function selectDirectory(path) {
      libraryForm.value.Directory = path;
      directoryBrowser.value = null;
      directoryError.value = '';
    }
    function saveLibrary() {
      return task(async () => {
        await api('/api/library', 'POST', libraryForm.value);
        await loadCatalog();
        await loadUsers();
        dialog.value.close();
      }, dialogError);
    }
    function deleteLibrary(lib) {
      if (!confirm('删除书库「' + lib.Name + '」？目录中的文件会保留。')) return;
      return task(async () => {
        await api('/api/library', 'DELETE', { id: lib.ID });
        await loadCatalog();
        await loadUsers();
      });
    }
    function editList() { listName.value = ''; showDialog('list'); }
    async function listAction(action, name, item) {
      catalog.value.lists = await (await api('/api/list', 'POST', { action, name, book: item?.ID })).json();
      await loadCatalog();
      if (page.value === 'search') await loadSearch();
    }
    function createList() {
      return task(async () => {
        await listAction('create', listName.value);
        if (selectedBook.value) showDialog('favorites');
        else dialog.value.close();
      }, dialogError);
    }
    function deleteList(name) {
      if (confirm('删除收藏清单「' + name + '」？')) return task(() => listAction('delete', name));
    }
    function chooseLists(item) { selectedBook.value = item; showDialog('favorites'); }
    function toggleList(name) {
      return task(() => listAction('toggle', name, selectedBook.value), dialogError);
    }
    function isFavorite(item) { return Object.values(catalog.value.lists).some(ids => ids.includes(item.ID)); }

    const sorted = computed(() => [...catalog.value.books].sort((a,b) => a.Title.localeCompare(b.Title, 'zh-CN') || a.Path.localeCompare(b.Path)));
    const folderRows = computed(() => {
      if (page.value !== 'library' || !library.value) return [];
      const children = new Map();
      for (const path of catalog.value.directories?.[library.value.ID] || []) {
        const segments = path.split('/');
        const name = segments.pop();
        const parent = segments.join('/');
        if (!children.has(parent)) children.set(parent, []);
        children.get(parent).push({ path, name });
      }
      const rows = [];
      function append(parent, depth) {
        const nodes = children.get(parent) || [];
        for (const node of nodes.sort((a, b) => a.name.localeCompare(b.name, 'zh-CN'))) {
          const href = link('library', library.value.ID, ...node.path.split('/'));
          const expanded = expandedFolders.value.has(href);
          rows.push({ ...node, href, depth, expanded, hasChildren: children.has(node.path) });
          if (expanded) append(node.path, depth + 1);
        }
      }
      append('', 0);
      return rows;
    });
    const currentFolder = computed(() => link(...parts.value));
    function toggleFolder(href) {
      if (expandedFolders.value.has(href)) expandedFolders.value.delete(href);
      else expandedFolders.value.add(href);
    }
    function groups(values, fallback) {
      const grouped = new Map();
      for (const item of sorted.value) {
        const names = [...new Set(values(item).map(name => name.trim()).filter(Boolean))];
        for (const name of names.length ? names : [fallback]) {
          if (!grouped.has(name)) grouped.set(name, []);
          grouped.get(name).push(item);
        }
      }
      return [...grouped].sort(([a],[b]) => a.localeCompare(b,'zh-CN')).map(([name,books]) => ({ name, books }));
    }
    const title = computed(() => ({
      home: '主页', favorites: '收藏', publishers: '出版社', authors: '作者',
      library: library.value?.Name || '书库', search: '超级搜索',
    })[page.value] || '主页');
    const sections = computed(() => {
      switch (page.value) {
        case 'favorites': return Object.entries(catalog.value.lists).map(([name, ids]) => ({ name, books: sorted.value.filter(b => ids.includes(b.ID)) }));
        case 'publishers': return groups(b => [b.Publisher], '未知出版社');
        case 'authors': return groups(b => b.Authors || [], '未知作者');
        case 'library': {
          const directory = parts.value.slice(2).join('/');
          return [{ books: sorted.value.filter(b => b.Library === library.value?.ID && b.Path.slice(0, Math.max(0, b.Path.lastIndexOf('/'))) === directory) }];
        }
        case 'search': return [{ books: searchBooks.value, empty: '没有找到匹配的书籍' }];
        case 'home': {
          const records = catalog.value.reading;
          const recent = sorted.value.filter(b => records[b.ID]).sort((a,b) => Date.parse(records[b.ID].Opened) - Date.parse(records[a.ID].Opened));
          const added = [...sorted.value].sort((a,b) => Date.parse(b.Added) - Date.parse(a.Added));
          return [{ name: '最近阅读', books: recent.slice(0,10), empty: '暂无阅读记录' }, { name:'最近新增', books: added.slice(0,10) }];
        }
        default: return [];
      }
    });
    function coverColor(item) {
      const colors = ['#253e37','#572b43','#2e3f5e','#83552f','#44532e','#783c30'];
      return colors[item.ID % colors.length];
    }
    function readingActive() {
      return document.visibilityState === 'visible' && document.hasFocus();
    }
    function savePosition(reading, keepalive = false) {
      if (reading !== activeReading || !signed.value) return;
      const now = performance.now();
      const elapsedMS = reading.since === null ? 0 : Math.min(30000, Math.round(now - reading.since));
      reading.since = readingActive() ? now : null;
      const payload = { id: reading.id, position: position.value, progress: progress.value, elapsedMS };
      // Keep writes for the current reading visit in the order that changes occurred.
      reading.pending = reading.pending.then(async () => {
        if (!signed.value || identity.value !== reading.identity) return;
        const record = await (await api('/api/reading', 'POST', payload, keepalive)).json();
        catalog.value.reading[reading.id] = record;
      }).catch(error => { if (activeReading === reading) notice.value = error.message; });
    }
    function setPage(value) {
      if (!activeReading || !Number.isInteger(value) || value < 0 || value >= pageCount.value) return;
      position.value = value;
      progress.value = Math.round(100 * (value + 1) / pageCount.value);
      savePosition(activeReading);
    }
    async function loadPreview(item, requestedPage, sheet, generation) {
      previewController?.abort();
      const controller = new AbortController();
      previewController = controller;
      previewBusy.value = true;
      previewError.value = '';
      const current = () => generation === readerGeneration && previewController === controller;
      async function fetchPage(page) {
        const params = new URLSearchParams({ page, sheet });
        const response = await api(bookURL(item, 'preview') + '?' + params, 'GET', undefined, false, controller.signal);
        return response.json();
      }
      try {
        let result = await fetchPage(requestedPage);
        if (!current()) return false;
        let table = result.table;
        if (table?.page > 1 && !table.rows.length && table.totalRows !== undefined) {
          const lastPage = Math.max(1, Math.ceil(table.totalRows / table.pageSize));
          if (lastPage < table.page) {
            result = await fetchPage(lastPage);
            if (!current()) return false;
            table = result.table;
          }
        }
        preview.value = result;
        if (table) {
          position.value = (table.sheets ? table.sheet * pagesPerSheet : 0) + table.page - 1;
          progress.value = table.totalRows > 0
            ? Math.min(100, Math.round(100 * table.page * table.pageSize / table.totalRows)) : null;
        } else if (result.type === 'slides') {
          position.value = (result.page || 1) - 1;
          progress.value = result.pages ? Math.round(100 * result.page / result.pages) : null;
        } else if (result.type === 'image') {
          position.value = 0;
          progress.value = 100;
        }
        return true;
      } catch (error) {
        if (current()) previewError.value = error.message;
        return false;
      } finally {
        if (current()) previewBusy.value = false;
      }
    }
    async function changePreview(page, sheet = preview.value?.table?.sheet || 0) {
      if (!book.value || !activeReading || previewBusy.value) return;
      const reading = activeReading;
      if (await loadPreview(book.value, page, sheet, readerGeneration)) savePosition(reading);
    }
    function textProgress() {
      const element = textReader.value;
      if (!element) return null;
      const range = element.scrollHeight - element.clientHeight;
      return range <= 0 ? 100 : Math.min(100, Math.max(0, Math.round(100 * element.scrollTop / range)));
    }
    function textScroll() {
      if (!activeReading || !textReader.value) return;
      position.value = Math.max(0, Math.round(textReader.value.scrollTop));
      progress.value = textProgress();
      clearTimeout(scrollTimer);
      const reading = activeReading;
      scrollTimer = setTimeout(() => savePosition(reading), 250);
    }
    function trackVisibility() {
      if (!activeReading) return;
      if (readingActive()) {
        if (activeReading.since === null) activeReading.since = performance.now();
      } else if (activeReading.since !== null) savePosition(activeReading, true);
    }
    function flushReading() {
      if (activeReading) {
        savePosition(activeReading, true);
        activeReading.since = null;
      }
    }
    watch(() => signed.value && page.value === 'read' ? book.value?.ID : null, async id => {
      clearTimeout(scrollTimer);
      flushReading();
      const lastSave = activeReading?.pending;
      if (activeReading) clearInterval(activeReading.timer);
      activeReading = null;
      const generation = ++readerGeneration;
      previewController?.abort();
      preview.value = null;
      previewBusy.value = false;
      previewError.value = '';
      pageCount.value = null;
      pdfFallback.value = false;
      progress.value = null;
      bookText.value = '';
      const item = id && book.value;
      if (!item) return;
      await lastSave;
      if (generation !== readerGeneration) return;
      const previous = catalog.value.reading[id];
      position.value = previous?.Position || 0;
      try {
        if (item.Format === 'epub' || item.Format === 'pdf') {
          const count = item.Format === 'pdf' ? item.Pages : await (await api(bookURL(item,'chapters'))).json();
          if (generation !== readerGeneration) return;
          if (count) {
            position.value = Math.min(position.value, count - 1);
            progress.value = Math.round(100 * (position.value + 1) / count);
          } else if (item.Format === 'pdf') pdfFallback.value = true;
          pageCount.value = count;
          if (count === 0 && item.Format === 'epub') return;
        } else if (item.Format === 'txt') {
          const content = await (await api(bookURL(item,'file'))).text();
          if (generation !== readerGeneration) return;
          bookText.value = content;
        } else {
          const tableOrSlides = ['csv', 'tsv', 'xlsx', 'parquet', 'pptx'].includes(item.Format);
          const sheet = item.Format === 'xlsx' ? Math.floor(position.value / pagesPerSheet) : 0;
          const index = item.Format === 'xlsx' ? position.value % pagesPerSheet : position.value;
          const start = tableOrSlides ? index + 1 : 1;
          if (!await loadPreview(item, start, sheet, generation) || generation !== readerGeneration) return;
        }
        if (item.Format === 'txt' || previewUsesScroll.value) {
          await nextTick();
          if (generation !== readerGeneration) return;
          if (textReader.value) {
            textReader.value.scrollTop = position.value;
            position.value = Math.round(textReader.value.scrollTop);
            progress.value = textProgress();
          }
        }
        const reading = { id, identity: identity.value, since: readingActive() ? performance.now() : null, pending: Promise.resolve() };
        activeReading = reading;
        savePosition(reading);
        reading.timer = setInterval(() => {
          if (readingActive()) savePosition(reading);
        }, 15000);
      } catch (error) { if (generation === readerGeneration) notice.value = error.message; }
    });
    readRoute();
    async function loadSession() {
      const session = await (await api('/api/session')).json();
      setup.value = session.setup;
      ldapEnabled.value = session.ldapEnabled;
      oidcEnabled.value = session.oidcEnabled;
      return session;
    }
    onMounted(async () => {
      window.addEventListener('hashchange', readRoute);
      window.addEventListener('focus', trackVisibility);
      window.addEventListener('blur', trackVisibility);
      window.addEventListener('pageshow', trackVisibility);
      window.addEventListener('pagehide', flushReading);
      document.addEventListener('visibilitychange', trackVisibility);
      try {
        const session = await loadSession();
        identity.value = session;
        if (session.authenticated) {
          await loadCatalog();
          signed.value = true;
          if (page.value === 'admin' && !admin.value) go('home');
        }
      } catch (error) { notice.value = error.message; }
      finally { ready.value = true; }
    });

    return {
      navigation, catalog, ready, signed, setup, busy, notice, query, password, currentPassword, newPassword,
      username, ldapEnabled, oidcEnabled, identity, admin, ldapForm, ldapMessage, ldapError, ldapAction,
      ldapTestUsername, ldapTestPassword,
      oidcForm, oidcMessage, oidcError, oidcAction,
      users, userForm, accessForm, editUser, editAccess, saveUser, saveAccess,
      imageVersion, account, dialog, dialogMode, dialogError, libraryForm, listName, selectedBook, parts,
      page, standalone, book, library, position, progress, pageCount, bookText, textReader, pdfFallback, title, sections,
      preview, previewBusy, previewError, previewUsesScroll, changePreview, formats,
      searchLibrary, searchFormat, searchOrder, searchBusy, searchError, searchMore, loadSearch, dateLabel, durationLabel,
      folderRows, currentFolder, toggleFolder,
      link, bookURL, coverColor, go, back, search, login, logout, changePassword, scan, scanLibrary, editLibrary, saveLibrary,
      directoryBrowser, directoryBusy, directoryError, browseDirectory, selectDirectory,
      deleteLibrary, editList, createList, deleteList, chooseLists, toggleList, isFavorite, setPage, textScroll,
      closeDialog: () => { dialogMode.value = ''; selectedBook.value = null; directoryBrowser.value = null; directoryError.value = ''; },
    };
  },
}).mount('#app');
