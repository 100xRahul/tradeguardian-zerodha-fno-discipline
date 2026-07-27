const csrf = document.querySelector('meta[name="csrf-token"]').content;
const byId = id => document.getElementById(id);
const money = paise => new Intl.NumberFormat('en-IN', {style: 'currency', currency: 'INR'}).format(paise / 100);
const rupees = value => new Intl.NumberFormat('en-IN', {style: 'currency', currency: 'INR'}).format(value);
const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, character => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;'}[character]));
const randomID = () => crypto.randomUUID().replaceAll('-', '');
const validPositiveNumber = value => Number.isFinite(Number(value)) && Number(value) > 0;
const LOSS_WARNING_PAISE = 2_000_000;
const LOCK_THRESHOLD_PAISE = 3_000_000;
const THEME_KEY = 'tg-theme';
const BASKET_NAME_KEY = 'tg-basket-name';
const WATCHLIST_KEY = 'tg-watchlist';
const KITE_LOGIN_PATH = '/auth/login';
const LOSS_WARNING_KEY = 'tg-loss-warning-shown';
const selectedPositions = new Set();
const orderSnapshots = new Map();
let modalResolver = null;
let latestPositions = [];
let kiteStatusKnown = false;
let activeWatchlistKey = '';

function positionKey(token, product) {
  return `${token}:${product}`;
}

function applyTheme(theme) {
  const next = theme === 'light' ? 'light' : 'dark';
  document.documentElement.dataset.theme = next;
  localStorage.setItem(THEME_KEY, next);
  byId('theme-toggle').textContent = next === 'light' ? 'Dark theme' : 'Light theme';
}

function initTheme() {
  const stored = localStorage.getItem(THEME_KEY);
  applyTheme(stored === 'light' ? 'light' : 'dark');
}

function initCollapsiblePanels() {
  document.querySelectorAll('[data-collapsible-toggle]').forEach(toggle => {
    const contentID = toggle.getAttribute('aria-controls');
    const content = contentID ? byId(contentID) : null;
    if (!content) return;
    const setExpanded = expanded => {
      toggle.setAttribute('aria-expanded', expanded ? 'true' : 'false');
      toggle.textContent = expanded ? 'Minimize' : 'Expand';
      content.hidden = !expanded;
    };
    setExpanded(false);
    toggle.addEventListener('click', () => {
      const expanded = toggle.getAttribute('aria-expanded') === 'true';
      setExpanded(!expanded);
    });
  });
}

function closeModal(result = false) {
  const root = byId('modal-root');
  root.classList.add('hidden');
  byId('modal-title').textContent = '';
  byId('modal-body').textContent = '';
  byId('modal-actions').innerHTML = '';
  byId('modal-root').querySelector('.modal-card').className = 'modal-card';
  const resolve = modalResolver;
  modalResolver = null;
  if (resolve) resolve(result);
}

function showModal({title, body, tone = '', actions = [{label: 'OK', primary: true}]}) {
  return new Promise(resolve => {
    if (modalResolver) closeModal(false);
    modalResolver = resolve;
    const card = byId('modal-root').querySelector('.modal-card');
    card.className = `modal-card${tone ? ` ${tone}` : ''}`;
    byId('modal-title').textContent = title;
    byId('modal-body').textContent = body;
    byId('modal-actions').innerHTML = actions.map((action, index) => {
      const classes = action.primary ? '' : 'secondary';
      return `<button type="button" class="${classes}" data-modal-action="${index}">${escapeHTML(action.label)}</button>`;
    }).join('');
    byId('modal-actions').onclick = event => {
      const button = event.target.closest('[data-modal-action]');
      if (!button) return;
      const action = actions[Number(button.dataset.modalAction)];
      if (action?.onClick) action.onClick();
      closeModal(action?.value ?? true);
    };
    byId('modal-root').classList.remove('hidden');
  });
}

function showConfirm({title, body, confirmLabel = 'Confirm', tone = ''}) {
  return showModal({
    title,
    body,
    tone,
    actions: [
      {label: 'Cancel', value: false},
      {label: confirmLabel, primary: true, value: true},
    ],
  });
}

function showSuccess(title, body) {
  return showModal({title, body, tone: 'success'});
}

function showWarning(title, body) {
  return showModal({title, body, tone: 'warning'});
}

function showError(title, body) {
  return showModal({title, body, tone: 'danger'});
}

byId('modal-root').addEventListener('click', event => {
  if (event.target.matches('[data-modal-dismiss]')) closeModal(false);
});
byId('theme-toggle').addEventListener('click', () => {
  applyTheme(document.documentElement.dataset.theme === 'light' ? 'dark' : 'light');
});
initTheme();
initCollapsiblePanels();

function rememberBasketName(value) {
  const trimmed = String(value ?? '').trim();
  if (trimmed) localStorage.setItem(BASKET_NAME_KEY, trimmed);
}

function restoreBasketName() {
  const field = byId('basket-name');
  if (!field) return;
  const saved = localStorage.getItem(BASKET_NAME_KEY);
  if (saved && !field.value) field.value = saved;
  field.addEventListener('input', () => rememberBasketName(field.value));
}

restoreBasketName();

function requireExecutionMetadata(instrument) {
  if (!Number.isInteger(instrument.instrument_token) || instrument.instrument_token <= 0) {
    throw new Error('Kite did not provide a valid instrument token. Refresh the catalogue before trading.');
  }
  if (!Number.isInteger(instrument.lot_size) || instrument.lot_size <= 0) {
    throw new Error('Kite did not provide a valid lot size. Refresh the catalogue before trading.');
  }
  if (!validPositiveNumber(instrument.tick_size)) {
    throw new Error('Kite did not provide a valid tick size. Refresh the catalogue before trading.');
  }
  if (instrument.instrument_type === 'CE' || instrument.instrument_type === 'PE') {
    const expiry = Date.parse(instrument.expiry);
    if (!instrument.name || !Number.isFinite(expiry) || !validPositiveNumber(instrument.strike)) {
      throw new Error('Kite did not provide complete option underlying, expiry, and strike metadata. This contract is blocked.');
    }
  }
  return instrument;
}

async function api(path, options = {}) {
  options.headers = {...(options.headers || {}), Accept: 'application/json'};
  if (options.method && options.method !== 'GET') {
    options.headers['Content-Type'] = 'application/json';
    options.headers['X-CSRF-Token'] = csrf;
  }
  const response = await fetch(path, options);
  const payload = await response.json();
  if (!response.ok) {
    const error = new Error(payload.error?.message || payload.decision?.message || 'Request failed');
    error.payload = payload;
    throw error;
  }
  return payload;
}

async function apiWithRetry(path, options = {}, attempts = 4) {
  let lastError = null;
  for (let attempt = 0; attempt < attempts; attempt++) {
    try {
      return await api(path, options);
    } catch (error) {
      lastError = error;
      if (attempt === attempts - 1) break;
      await new Promise(resolve => setTimeout(resolve, 400 * (attempt + 1)));
    }
  }
  throw lastError;
}

function watchlistStorageKey(instrument) {
  return `${instrument.exchange}:${instrument.tradingsymbol}`;
}

function loadWatchlist() {
  try {
    const saved = JSON.parse(localStorage.getItem(WATCHLIST_KEY) || '[]');
    return Array.isArray(saved) ? saved.filter(item => item?.exchange && item?.tradingsymbol) : [];
  } catch (_error) {
    return [];
  }
}

function saveWatchlist(items) {
  localStorage.setItem(WATCHLIST_KEY, JSON.stringify(items));
}

function addToWatchlist(instrument) {
  const items = loadWatchlist();
  const key = watchlistStorageKey(instrument);
  if (items.some(item => watchlistStorageKey(item) === key)) return false;
  items.push(instrument);
  saveWatchlist(items);
  renderWatchlist();
  return true;
}

function removeFromWatchlist(key) {
  const items = loadWatchlist().filter(item => watchlistStorageKey(item) !== key);
  saveWatchlist(items);
  if (activeWatchlistKey === key) activeWatchlistKey = '';
  renderWatchlist();
}

function renderWatchlist() {
  const items = loadWatchlist();
  const container = byId('watchlist-items');
  byId('watchlist-count').textContent = `${items.length} contract${items.length === 1 ? '' : 's'}`;
  if (!items.length) {
    container.innerHTML = '<p class="empty watchlist-empty">Search above to add contracts. Click a saved contract to load it in the order ticket.</p>';
    return;
  }
  container.innerHTML = items.map(instrument => {
    const key = watchlistStorageKey(instrument);
    const active = key === activeWatchlistKey ? ' active' : '';
    return `<button type="button" class="watchlist-chip${active}" data-watchlist-select="${escapeHTML(key)}"><strong>${escapeHTML(instrument.exchange)}:${escapeHTML(instrument.tradingsymbol)}</strong><span>${escapeHTML(instrumentDescription(instrument))}</span><span class="watchlist-remove danger-button small" data-watchlist-remove="${escapeHTML(key)}" role="button" aria-label="Remove ${escapeHTML(instrument.tradingsymbol)}">×</span></button>`;
  }).join('');
}

function applyInstrumentToOrderTicket(instrument) {
  if (!instrument) return;
  const exchangeField = byId('order-exchange');
  if (exchangeField.value !== instrument.exchange) {
    exchangeField.value = instrument.exchange;
    orderPicker.clear();
  }
  orderPicker.select(instrument);
  activeWatchlistKey = watchlistStorageKey(instrument);
  renderWatchlist();
  byId('order-form').scrollIntoView({behavior: 'smooth', block: 'nearest'});
}

function instrumentDescription(instrument) {
  const expiry = instrument.expiry && !instrument.expiry.startsWith('0001-')
    ? new Date(instrument.expiry).toLocaleDateString('en-IN', {day: '2-digit', month: 'short', year: 'numeric'})
    : 'No expiry';
  const contract = instrument.instrument_type === 'FUT'
    ? 'Future'
    : `${Number(instrument.strike).toLocaleString('en-IN')} ${instrument.instrument_type}`;
  const underlying = instrument.name ? instrument.name : 'Underlying not supplied';
  const lot = Number.isInteger(instrument.lot_size) && instrument.lot_size > 0 ? instrument.lot_size : 'Unavailable';
  const tick = validPositiveNumber(instrument.tick_size) ? instrument.tick_size : 'Unavailable';
  return `${instrument.exchange} · ${underlying} · ${expiry} · ${contract} · lot ${lot} · tick ${tick}`;
}

class InstrumentPicker {
  constructor(root, exchange, options = {}) {
    this.root = root;
    this.exchange = exchange;
    this.optionsOnly = Boolean(options.optionsOnly);
    this.onSelect = options.onSelect || (() => {});
    this.input = root.querySelector('input[type="search"]');
    this.results = root.querySelector('.instrument-results');
    this.summary = root.querySelector('.selected-instrument');
    this.selected = null;
    this.timer = null;
    this.controller = null;
    this.requestNumber = 0;
    this.input.addEventListener('input', () => this.handleInput());
    this.input.addEventListener('keydown', event => {
      if (event.key === 'Escape') this.close();
    });
    this.results.addEventListener('click', event => {
      const button = event.target.closest('[data-instrument-index]');
      if (button) this.select(this.matches[Number(button.dataset.instrumentIndex)]);
    });
  }

  handleInput() {
    this.selected = null;
    if (this.summary) {
      this.summary.textContent = '';
      this.summary.className = 'selected-instrument';
    }
    this.onSelect(null);
    clearTimeout(this.timer);
    if (this.controller) this.controller.abort();
    const query = this.input.value.trim();
    if (query.length < 2) {
      this.results.innerHTML = query ? '<span class="search-message">Type at least 2 characters</span>' : '';
      this.input.setAttribute('aria-expanded', 'false');
      return;
    }
    this.results.innerHTML = '<span class="search-message">Searching contracts…</span>';
    this.input.setAttribute('aria-expanded', 'true');
    this.timer = setTimeout(() => this.search(query), 60);
  }

  async search(query) {
    const requestNumber = ++this.requestNumber;
    this.controller = new AbortController();
    const kind = this.optionsOnly ? '&kind=OPTION' : '';
    try {
      const data = await api(`/api/instruments?q=${encodeURIComponent(query)}&exchange=${encodeURIComponent(this.exchange())}${kind}`, {signal: this.controller.signal});
      if (requestNumber !== this.requestNumber) return;
      this.matches = data.instruments || [];
      this.results.innerHTML = this.matches.length
        ? this.matches.map((instrument, index) => `<button type="button" role="option" data-instrument-index="${index}"><strong>${escapeHTML(instrument.tradingsymbol)}</strong><span>${escapeHTML(instrumentDescription(instrument))}</span></button>`).join('')
        : '<span class="search-message">No matching contracts. Connect Kite if instruments are not loaded.</span>';
      this.input.setAttribute('aria-expanded', 'true');
    } catch (error) {
      if (error.name === 'AbortError') return;
      this.results.innerHTML = `<span class="search-message error">${escapeHTML(error.message)}</span>`;
    }
  }

  select(instrument) {
    if (!instrument) return;
    this.selected = instrument;
    this.input.value = instrument.tradingsymbol;
    if (this.summary) {
      this.summary.textContent = instrumentDescription(instrument);
      this.summary.className = 'selected-instrument visible';
    }
    this.close();
    this.onSelect(instrument);
  }

  clear() {
    this.requestNumber++;
    clearTimeout(this.timer);
    if (this.controller) this.controller.abort();
    this.selected = null;
    this.input.value = '';
    if (this.summary) {
      this.summary.textContent = '';
      this.summary.className = 'selected-instrument';
    }
    this.close();
    this.onSelect(null);
  }

  close() {
    this.results.innerHTML = '';
    this.input.setAttribute('aria-expanded', 'false');
  }

  requireSelection() {
    if (!this.selected) throw new Error('Search for a contract and select it from the results.');
    return this.selected;
  }
}

function renderDashboard(state) {
  renderStatus(state.status);
  renderPositions(state.positions);
  renderOrders(state.orders);
  maybeWarnLoss(state.status);
}

function maybeWarnLoss(status) {
  const hasLiveMTM = Number.isFinite(status.live_mtm_paise);
  const displayedMTM = hasLiveMTM ? status.live_mtm_paise : status.mtm_paise;
  if (!Number.isFinite(displayedMTM) || displayedMTM > -LOSS_WARNING_PAISE || displayedMTM <= -LOCK_THRESHOLD_PAISE) return;
  if (sessionStorage.getItem(LOSS_WARNING_KEY) === '1') return;
  sessionStorage.setItem(LOSS_WARNING_KEY, '1');
  showWarning(
    'Loss approaching lock threshold',
    `F&O MTM is ${money(displayedMTM)}. Trading locks automatically at ${money(-LOCK_THRESHOLD_PAISE)}. Review open risk before placing new orders.`,
  );
}

async function refreshDashboard() {
  renderDashboard(await apiWithRetry('/api/dashboard'));
}

async function refreshAudit() {
  const audit = await apiWithRetry('/api/audit');
  renderAudit(audit.events);
}

function renderKiteStatus(status, known = kiteStatusKnown) {
  const kiteConnect = byId('kite-connect');
  kiteConnect.classList.remove('connected', 'actionable');
  kiteConnect.removeAttribute('aria-disabled');
  if (!known) {
    kiteConnect.textContent = 'Checking Kite session…';
    kiteConnect.href = KITE_LOGIN_PATH;
    kiteConnect.setAttribute('aria-disabled', 'true');
    kiteConnect.classList.add('connected');
    return;
  }
  if (status.authenticated) {
    const runtime = status.runtime_status === 'READY' ? 'session active' : status.runtime_status.replaceAll('_', ' ').toLowerCase();
    kiteConnect.textContent = `Kite connected · ${runtime}`;
    kiteConnect.removeAttribute('href');
    kiteConnect.classList.add('connected');
    kiteConnect.setAttribute('aria-disabled', 'true');
    return;
  }
  kiteConnect.textContent = 'Connect Kite';
  kiteConnect.href = KITE_LOGIN_PATH;
  kiteConnect.classList.add('actionable');
}

function renderStatus(status) {
  const hasLiveMTM = Number.isFinite(status.live_mtm_paise);
  const displayedMTM = hasLiveMTM ? status.live_mtm_paise : status.mtm_paise;
  byId('mtm-label').textContent = hasLiveMTM ? 'Live F&O daily MTM · risk value' : 'F&O MTM · delayed REST snapshot';
  byId('mtm').textContent = money(displayedMTM);
  byId('mtm').className = displayedMTM <= -LOCK_THRESHOLD_PAISE ? 'negative' : displayedMTM <= -LOSS_WARNING_PAISE ? 'warning' : displayedMTM < 0 ? 'warning' : 'positive';
  byId('confirmed-mtm').textContent = money(status.mtm_paise);
  byId('market-feed').textContent = status.market_data_status === 'LIVE' && status.market_data_at
    ? `Live · ${new Date(status.market_data_at).toLocaleTimeString('en-IN')}`
    : (status.market_data_status || 'DISCONNECTED').replaceAll('_', ' ');
  byId('trading').textContent = status.trading_status;
  byId('runtime').textContent = status.runtime_status;
  byId('refresh').textContent = status.last_refresh ? new Date(status.last_refresh).toLocaleTimeString('en-IN') : 'Waiting';
  byId('open-qty').textContent = status.open_position_quantity;
  byId('pending-count').textContent = status.pending_orders;
  byId('liquidation').textContent = status.liquidation_state || 'Not active';
  byId('unlock').textContent = status.next_unlock ? new Date(status.next_unlock).toLocaleString('en-IN') : '—';
  byId('last-error').textContent = status.last_error || '';
  const alert = byId('alert');
  alert.textContent = status.message;
  alert.className = `alert ${status.trading_status === 'LOCKED' ? 'danger' : status.runtime_status === 'READY' ? 'safe' : 'warning'}`;
  byId('submit-order').disabled = status.trading_status === 'LOCKED' || status.runtime_status !== 'READY';
  byId('submit-basket').disabled = status.trading_status === 'LOCKED' || status.runtime_status !== 'READY';
  kiteStatusKnown = true;
  renderKiteStatus(status);
}

function syncPositionSelectionUI() {
  const selectable = latestPositions.filter(position => position.quantity);
  const exitSelected = byId('exit-selected');
  exitSelected.hidden = selectedPositions.size === 0;
  exitSelected.textContent = selectedPositions.size > 1
    ? `Exit ${selectedPositions.size} selected`
    : 'Exit selected';
  const selectAll = byId('select-all-positions');
  selectAll.disabled = selectable.length === 0;
  selectAll.checked = selectable.length > 0 && selectable.every(position => selectedPositions.has(positionKey(position.instrument_token, position.product)));
  selectAll.indeterminate = selectedPositions.size > 0 && !selectAll.checked;
}

function renderPositions(rows = []) {
  latestPositions = rows.filter(position => position.quantity);
  const validKeys = new Set(latestPositions.map(position => positionKey(position.instrument_token, position.product)));
  for (const key of [...selectedPositions]) {
    if (!validKeys.has(key)) selectedPositions.delete(key);
  }
  byId('positions').innerHTML = latestPositions.length ? latestPositions.map(position => {
    const key = positionKey(position.instrument_token, position.product);
    const selectable = Boolean(position.quantity);
    const checked = selectedPositions.has(key) ? 'checked' : '';
    const checkbox = selectable
      ? `<input type="checkbox" data-position-select value="${escapeHTML(key)}" ${checked} aria-label="Select ${escapeHTML(position.tradingsymbol)}">`
      : '';
    const action = !selectable ? '' : `<button type="button" class="danger-button small" data-exit="${position.instrument_token}" data-exit-product="${escapeHTML(position.product)}" data-exit-symbol="${escapeHTML(position.exchange)}:${escapeHTML(position.tradingsymbol)}" data-exit-qty="${Math.abs(position.quantity)}">Exit</button>`;
    return `<tr><td class="select-col">${checkbox}</td><td><strong>${escapeHTML(position.exchange)}:${escapeHTML(position.tradingsymbol)}</strong></td><td>${escapeHTML(position.product)}</td><td>${position.quantity}</td><td>${rupees(position.last_price)}</td><td class="${position.m2m < 0 ? 'negative' : 'positive'}">${rupees(position.m2m)}</td><td>${action}</td></tr>`;
  }).join('') : '<tr><td colspan="7" class="empty">No F&amp;O positions</td></tr>';
  syncPositionSelectionUI();
}

function detectOrderFillChanges(rows = []) {
  for (const order of rows) {
    const previous = orderSnapshots.get(order.order_id);
    const filled = Number(order.filled_quantity) || 0;
    const quantity = Number(order.quantity) || 0;
    const pending = Number(order.pending_quantity) || 0;
    if (previous) {
      const newlyFilled = filled - previous.filled;
      if (newlyFilled > 0 && pending > 0) {
        showWarning(
          'Partial fill in progress',
          `${order.exchange}:${order.tradingsymbol} filled ${newlyFilled} of ${quantity}. ${pending} quantity is still pending at Kite.`,
        );
      } else if (
        previous.status !== 'COMPLETE'
        && order.status === 'COMPLETE'
        && filled > 0
        && filled < quantity
      ) {
        showWarning(
          'Order partially filled',
          `${order.exchange}:${order.tradingsymbol} completed with ${filled} of ${quantity} filled. Review the order book before assuming the full size executed.`,
        );
      }
    }
    orderSnapshots.set(order.order_id, {filled, quantity, status: order.status});
  }
}

function renderOrders(rows = []) {
  detectOrderFillChanges(rows);
  const terminal = new Set(['COMPLETE', 'CANCELLED', 'REJECTED']);
  byId('orders').innerHTML = rows.length ? rows.map(order => {
    const cancellable = !terminal.has(order.status);
    return `<tr><td class="mono">${escapeHTML(order.order_id)}</td><td>${escapeHTML(order.exchange)}:${escapeHTML(order.tradingsymbol)}</td><td>${escapeHTML(order.transaction_type)}</td><td>${escapeHTML(order.order_type)}</td><td>${order.quantity} / ${order.pending_quantity}</td><td>${escapeHTML(order.status)}</td><td>${cancellable ? `<button type="button" class="secondary small" data-modify="${escapeHTML(order.order_id)}">Modify</button> <button type="button" class="danger-button small" data-cancel="${escapeHTML(order.order_id)}">Cancel</button>` : ''}</td></tr>`;
  }).join('') : '<tr><td colspan="7" class="empty">No orders today</td></tr>';
}

function renderAudit(rows = []) {
  byId('audit').innerHTML = rows.length ? rows.map(event => `<tr><td>${new Date(event.created_at).toLocaleTimeString('en-IN')}</td><td>${escapeHTML(event.type)}</td><td>${escapeHTML(event.code)}</td><td>${escapeHTML(event.message)}</td></tr>`).join('') : '<tr><td colspan="4" class="empty">No audit events</td></tr>';
}

const orderForm = byId('order-form');
const orderLots = orderForm.elements.lots;
const orderPicker = new InstrumentPicker(byId('order-instrument'), () => byId('order-exchange').value, {
  onSelect: instrument => {
    const lots = Number(orderLots.value) || 0;
    byId('order-quantity').textContent = instrument ? `${lots} lot${lots === 1 ? '' : 's'} = ${lots * instrument.lot_size} quantity` : 'Select a contract to calculate quantity';
    const tick = instrument && validPositiveNumber(instrument.tick_size) ? String(instrument.tick_size) : '';
    for (const field of [byId('order-price'), byId('order-trigger')]) {
      if (tick) {
        field.step = tick;
        field.min = tick;
      } else {
        field.removeAttribute('step');
        field.removeAttribute('min');
      }
    }
  }
});

orderLots.addEventListener('input', () => orderPicker.onSelect(orderPicker.selected));
byId('order-exchange').addEventListener('change', () => {
  activeWatchlistKey = '';
  renderWatchlist();
  orderPicker.clear();
});

const watchlistPicker = new InstrumentPicker(byId('watchlist-picker'), () => byId('watchlist-exchange').value, {
  onSelect: instrument => {
    if (!instrument) return;
    addToWatchlist(instrument);
    applyInstrumentToOrderTicket(instrument);
    watchlistPicker.input.value = '';
    watchlistPicker.selected = null;
    watchlistPicker.close();
  },
});
byId('watchlist-exchange').addEventListener('change', () => watchlistPicker.clear());
byId('watchlist-items').addEventListener('click', event => {
  const remove = event.target.closest('[data-watchlist-remove]');
  if (remove) {
    event.stopPropagation();
    removeFromWatchlist(remove.dataset.watchlistRemove);
    return;
  }
  const select = event.target.closest('[data-watchlist-select]');
  if (!select) return;
  const key = select.dataset.watchlistSelect;
  const instrument = loadWatchlist().find(item => watchlistStorageKey(item) === key);
  if (instrument) applyInstrumentToOrderTicket(instrument);
});
renderWatchlist();

function syncOrderType() {
  const type = byId('order-type').value;
  const priceEnabled = type === 'LIMIT' || type === 'SL';
  const triggerEnabled = type === 'SL' || type === 'SL-M';
  byId('order-price').disabled = !priceEnabled;
  byId('order-trigger').disabled = !triggerEnabled;
  byId('order-price').required = priceEnabled;
  byId('order-trigger').required = triggerEnabled;
  if (!priceEnabled) byId('order-price').value = '0';
  if (!triggerEnabled) byId('order-trigger').value = '0';
}
byId('order-type').addEventListener('change', syncOrderType);
syncOrderType();

orderForm.addEventListener('submit', async event => {
  event.preventDefault();
  const output = byId('order-result');
  output.textContent = 'Checking rules…';
  output.className = '';
  try {
    const instrument = requireExecutionMetadata(orderPicker.requireSelection());
    const form = new FormData(orderForm);
    const lots = Number(form.get('lots'));
    if (!Number.isInteger(lots) || lots < 1) throw new Error('Lots must be a whole number of at least 1.');
    const body = {
      idempotency_key: randomID(), variety: 'regular', validity: 'DAY',
      exchange: instrument.exchange, tradingsymbol: instrument.tradingsymbol,
      transaction_type: form.get('transaction_type'), product: form.get('product'),
      order_type: form.get('order_type'), quantity: lots * instrument.lot_size,
      price: Number(byId('order-price').value), trigger_price: Number(byId('order-trigger').value)
    };
    const result = await api('/api/orders', {method: 'POST', body: JSON.stringify(body)});
    output.textContent = `${result.decision.message} Order: ${result.order_id}`;
    output.className = 'success';
    await showSuccess(
      'Order submitted',
      `${result.decision.message}\nOrder ID: ${result.order_id}\n${instrument.exchange}:${instrument.tradingsymbol} · ${body.transaction_type} · ${body.quantity} qty · ${body.product}`,
    );
  } catch (error) {
    output.textContent = error.payload?.decision?.message || error.message;
    output.className = 'error';
    await showError('Order rejected', error.payload?.decision?.message || error.message);
  }
  await Promise.allSettled([refreshDashboard(), refreshAudit()]);
});

const basketPickers = new Map();
function addBasketLeg(side = 'BUY') {
  const rows = byId('basket-legs');
  if (rows.children.length >= 4) return;
  const row = document.createElement('div');
  row.className = 'basket-leg';
  const resultID = `instrument-results-${randomID()}`;
  row.innerHTML = `<label>Side<select data-field="transaction_type"><option ${side === 'BUY' ? 'selected' : ''}>BUY</option><option ${side === 'SELL' ? 'selected' : ''}>SELL</option></select></label><label>Option contract<span class="instrument-picker"><input type="search" autocomplete="off" role="combobox" aria-expanded="false" aria-controls="${resultID}" placeholder="Search underlying or strike"><span class="instrument-results" id="${resultID}" role="listbox"></span><span class="selected-instrument"></span></span></label><label>Lots<input data-field="lots" type="number" min="1" step="1" value="1"><span class="field-help" data-quantity>Select contract</span></label><label data-price-label>IOC limit price<input data-field="limit_price" type="number"></label><button type="button" class="danger-button small" data-remove-leg>Delete</button>`;
  rows.appendChild(row);
  const picker = new InstrumentPicker(row.querySelector('.instrument-picker'), () => byId('basket-exchange').value, {
    optionsOnly: true,
    onSelect: instrument => {
      const lots = Number(row.querySelector('[data-field="lots"]').value) || 0;
      row.querySelector('[data-quantity]').textContent = instrument ? `${lots} lot${lots === 1 ? '' : 's'} = ${lots * instrument.lot_size} quantity` : 'Select contract';
      const price = row.querySelector('[data-field="limit_price"]');
      if (instrument && validPositiveNumber(instrument.tick_size)) {
        price.step = String(instrument.tick_size);
        price.min = String(instrument.tick_size);
      } else {
        price.removeAttribute('step');
        price.removeAttribute('min');
      }
    }
  });
  row.querySelector('[data-field="lots"]').addEventListener('input', () => picker.onSelect(picker.selected));
  basketPickers.set(row, picker);
  syncBasketOrderType();
}

function syncBasketOrderType() {
  const market = byId('basket-order-type')?.value === 'MARKET';
  document.querySelectorAll('.basket-leg').forEach(row => {
    const label = row.querySelector('[data-price-label]');
    const price = row.querySelector('[data-field="limit_price"]');
    label.childNodes[0].textContent = market ? 'Market price' : 'IOC limit price';
    price.disabled = market;
    if (market) price.value = '';
  });
}

addBasketLeg('BUY');
addBasketLeg('SELL');
byId('add-leg').addEventListener('click', () => addBasketLeg('SELL'));
byId('basket-exchange').addEventListener('change', () => basketPickers.forEach(picker => picker.clear()));
byId('basket-order-type').addEventListener('change', syncBasketOrderType);
byId('basket-legs').addEventListener('click', event => {
  if (!event.target.matches('[data-remove-leg]')) return;
  const row = event.target.closest('.basket-leg');
  basketPickers.delete(row);
  row.remove();
});

byId('basket-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const exchange = form.get('basket_exchange');
  const product = form.get('basket_product');
  const orderType = form.get('basket_order_type');
  const output = byId('basket-result');
  output.textContent = 'Validating complete portfolio…';
  output.className = '';
  try {
    const basketName = String(form.get('basket_name') ?? byId('basket-name').value ?? '').trim();
    if (!basketName) throw new Error('Enter a basket name before deploying.');
    rememberBasketName(basketName);
    const legs = [...byId('basket-legs').children].map(row => {
      const selected = basketPickers.get(row).selected;
      if (!selected) return null;
      const instrument = requireExecutionMetadata(selected);
      const lots = Number(row.querySelector('[data-field="lots"]').value);
      if (!Number.isInteger(lots) || lots < 1) throw new Error('Every leg must use a whole number of lots.');
      const limitPrice = orderType === 'LIMIT' ? Number(row.querySelector('[data-field="limit_price"]').value) : 0;
      if (orderType === 'LIMIT' && !validPositiveNumber(limitPrice)) throw new Error('Every selected LIMIT leg requires a positive price.');
      return {
        exchange, product, tradingsymbol: instrument.tradingsymbol,
        transaction_type: row.querySelector('[data-field="transaction_type"]').value,
        quantity: lots * instrument.lot_size,
        limit_price: limitPrice
      };
    }).filter(Boolean);
    if (legs.length < 2) throw new Error('Select at least two option contracts. Blank rows are ignored.');
    const data = await api('/api/baskets', {method: 'POST', body: JSON.stringify({idempotency_key: randomID(), name: basketName, order_type: orderType, legs})});
    const loss = data.result.max_loss_known ? ` Maximum planned loss: ${money(data.result.max_loss_paise)}.` : ' Maximum planned loss is unavailable for MARKET execution because fill prices are not known in advance.';
    output.textContent = data.result.message + loss;
    output.className = 'success';
    await showSuccess('Basket submitted', data.result.message + loss);
  } catch (error) {
    output.textContent = error.payload?.result?.message || error.message;
    output.className = 'error';
    await showError('Basket rejected', error.payload?.result?.message || error.message);
  }
  await Promise.allSettled([refreshDashboard(), refreshAudit()]);
});

async function exitPosition({token, product, symbol, quantity}) {
  const confirmed = await showConfirm({
    title: 'Exit position',
    body: `Exit ${symbol} (${product}) at market with Kite automatic protection?\nQuantity to exit: ${quantity}`,
    confirmLabel: 'Exit position',
    tone: 'warning',
  });
  if (!confirmed) return;
  try {
    const result = await api(`/api/positions/${encodeURIComponent(token)}/exit`, {
      method: 'POST',
      body: JSON.stringify({product, quantity}),
    });
    await showSuccess('Exit submitted', `${symbol} exit order submitted.\nOrder ID: ${result.order_id || 'pending reconciliation'}`);
  } catch (error) {
    await showError('Exit failed', error.payload?.error?.message || error.message);
  }
  await Promise.allSettled([refreshDashboard(), refreshAudit()]);
}

async function exitSelectedPositions() {
  const targets = latestPositions.filter(position => {
    const key = positionKey(position.instrument_token, position.product);
    return position.quantity && selectedPositions.has(key);
  });
  if (!targets.length) return;
  const confirmed = await showConfirm({
    title: 'Exit selected positions',
    body: `Exit ${targets.length} selected F&O position${targets.length === 1 ? '' : 's'} at market with Kite automatic protection?`,
    confirmLabel: `Exit ${targets.length} position${targets.length === 1 ? '' : 's'}`,
    tone: 'warning',
  });
  if (!confirmed) return;
  const failures = [];
  const successes = [];
  for (const position of targets) {
    try {
      const result = await api(`/api/positions/${encodeURIComponent(position.instrument_token)}/exit`, {
        method: 'POST',
        body: JSON.stringify({product: position.product}),
      });
      successes.push(`${position.exchange}:${position.tradingsymbol} (${result.order_id || 'submitted'})`);
      selectedPositions.delete(positionKey(position.instrument_token, position.product));
    } catch (error) {
      failures.push(`${position.exchange}:${position.tradingsymbol}: ${error.payload?.error?.message || error.message}`);
    }
  }
  if (successes.length) {
    await showSuccess(
      successes.length === 1 ? 'Exit submitted' : 'Exits submitted',
      successes.join('\n'),
    );
  }
  if (failures.length) {
    await showError(
      failures.length === 1 ? 'Exit failed' : 'Some exits failed',
      failures.join('\n'),
    );
  }
  await Promise.allSettled([refreshDashboard(), refreshAudit()]);
}

byId('positions').addEventListener('change', event => {
  if (event.target.matches('[data-position-select]')) {
    const key = event.target.value;
    if (event.target.checked) selectedPositions.add(key);
    else selectedPositions.delete(key);
    syncPositionSelectionUI();
    return;
  }
});

byId('select-all-positions').addEventListener('change', event => {
  const selectable = latestPositions.filter(position => position.quantity);
  selectedPositions.clear();
  if (event.target.checked) {
    for (const position of selectable) {
      selectedPositions.add(positionKey(position.instrument_token, position.product));
    }
  }
  renderPositions(latestPositions);
});

byId('exit-selected').addEventListener('click', () => {
  exitSelectedPositions();
});

document.addEventListener('click', async event => {
  if (!event.target.closest('.instrument-picker')) document.querySelectorAll('.instrument-picker').forEach(picker => {
    picker.querySelector('.instrument-results').innerHTML = '';
    picker.querySelector('input[type="search"]').setAttribute('aria-expanded', 'false');
  });
  const cancelButton = event.target.closest('[data-cancel]');
  if (cancelButton) {
    const cancel = cancelButton.dataset.cancel;
    if (!(await showConfirm({
      title: 'Cancel order',
      body: `Cancel pending order ${cancel}?`,
      confirmLabel: 'Cancel order',
      tone: 'warning',
    }))) return;
    try {
      await api(`/api/orders/${encodeURIComponent(cancel)}/cancel`, {method: 'POST', body: '{}'});
      await showSuccess('Order cancelled', `Cancellation submitted for order ${cancel}.`);
    } catch (error) {
      await showError('Cancel failed', error.payload?.error?.message || error.message);
    }
    await Promise.allSettled([refreshDashboard(), refreshAudit()]);
    return;
  }
  const exitButton = event.target.closest('[data-exit]');
  if (exitButton) {
    const fullQuantity = Number(exitButton.dataset.exitQty);
    const input = prompt(`Quantity to exit (max ${fullQuantity}):`, String(fullQuantity));
    if (input === null) return;
    const quantity = Number(input);
    if (!Number.isInteger(quantity) || quantity <= 0 || quantity > fullQuantity) {
      await showError('Invalid quantity', `Enter a whole number between 1 and ${fullQuantity}.`);
      return;
    }
    await exitPosition({
      token: exitButton.dataset.exit,
      product: exitButton.dataset.exitProduct,
      symbol: exitButton.dataset.exitSymbol,
      quantity,
    });
    return;
  }
  const modifyButton = event.target.closest('[data-modify]');
  if (modifyButton) {
    const modify = modifyButton.dataset.modify;
    const quantity = Number(prompt('New total quantity:'));
    if (!quantity) return;
    const orderType = (prompt('Order type (MARKET, LIMIT, SL, SL-M):', 'LIMIT') || '').toUpperCase();
    if (!orderType) return;
    const price = Number(prompt('Price (0 when unused):', '0'));
    const triggerPrice = Number(prompt('Trigger price (0 when unused):', '0'));
    try {
      await api(`/api/orders/${encodeURIComponent(modify)}/modify`, {method: 'POST', body: JSON.stringify({idempotency_key: randomID(), quantity, order_type: orderType, validity: 'DAY', price, trigger_price: triggerPrice})});
      await showSuccess('Order modified', `Modification submitted for order ${modify}.`);
    } catch (error) {
      await showError('Modify failed', error.payload?.error?.message || error.message);
    }
    await Promise.allSettled([refreshDashboard(), refreshAudit()]);
  }
});

const events = new EventSource('/api/events');
events.addEventListener('open', () => {
  byId('live-connection').textContent = 'Live';
  refreshDashboard().catch(() => {});
});
events.addEventListener('state', event => {
  try {
    renderDashboard(JSON.parse(event.data));
  } catch (_error) {
    byId('live-connection').textContent = 'Update error';
  }
});
events.addEventListener('error', () => {
  byId('live-connection').textContent = 'Reconnecting…';
});

Promise.all([refreshDashboard(), refreshAudit()]).catch(error => {
  byId('alert').textContent = error.message;
  byId('alert').className = 'alert danger';
  renderKiteStatus({authenticated: false, runtime_status: 'AUTH_REQUIRED'}, false);
});
