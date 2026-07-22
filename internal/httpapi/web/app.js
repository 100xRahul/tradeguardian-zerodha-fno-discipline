const csrf = document.querySelector('meta[name="csrf-token"]').content;
const byId = id => document.getElementById(id);
const money = paise => new Intl.NumberFormat('en-IN', {style: 'currency', currency: 'INR'}).format(paise / 100);
const rupees = value => new Intl.NumberFormat('en-IN', {style: 'currency', currency: 'INR'}).format(value);
const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, character => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;'}[character]));
const randomID = () => crypto.randomUUID().replaceAll('-', '');
const validPositiveNumber = value => Number.isFinite(Number(value)) && Number(value) > 0;

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
    this.summary.textContent = '';
    this.summary.className = 'selected-instrument';
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
    this.summary.textContent = instrumentDescription(instrument);
    this.summary.className = 'selected-instrument visible';
    this.close();
    this.onSelect(instrument);
  }

  clear() {
    this.requestNumber++;
    clearTimeout(this.timer);
    if (this.controller) this.controller.abort();
    this.selected = null;
    this.input.value = '';
    this.summary.textContent = '';
    this.summary.className = 'selected-instrument';
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
}

async function refreshDashboard() {
  renderDashboard(await api('/api/dashboard'));
}

async function refreshAudit() {
  const audit = await api('/api/audit');
  renderAudit(audit.events);
}

function renderStatus(status) {
  const hasLiveMTM = Number.isFinite(status.live_mtm_paise);
  const displayedMTM = hasLiveMTM ? status.live_mtm_paise : status.mtm_paise;
  byId('mtm-label').textContent = hasLiveMTM ? 'Live F&O daily MTM · risk value' : 'F&O MTM · delayed REST snapshot';
  byId('mtm').textContent = money(displayedMTM);
  byId('mtm').className = displayedMTM <= -3000000 ? 'negative' : displayedMTM < 0 ? 'warning' : 'positive';
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
  const kiteConnect = byId('kite-connect');
  if (status.authenticated) {
    kiteConnect.textContent = 'Kite connected';
    kiteConnect.removeAttribute('href');
    kiteConnect.setAttribute('aria-disabled', 'true');
  } else {
    kiteConnect.textContent = 'Connect Kite';
    kiteConnect.href = kiteConnect.dataset.loginHref;
    kiteConnect.removeAttribute('aria-disabled');
  }
}

function renderPositions(rows = []) {
  byId('positions').innerHTML = rows.length ? rows.map(position => {
    const action = !position.quantity ? '' : `<button class="danger-button small" data-exit="${position.instrument_token}" data-exit-product="${escapeHTML(position.product)}">Exit</button>`;
    return `<tr><td><strong>${escapeHTML(position.exchange)}:${escapeHTML(position.tradingsymbol)}</strong></td><td>${escapeHTML(position.product)}</td><td>${position.quantity}</td><td>${rupees(position.last_price)}</td><td class="${position.m2m < 0 ? 'negative' : 'positive'}">${rupees(position.m2m)}</td><td>${action}</td></tr>`;
  }).join('') : '<tr><td colspan="6" class="empty">No F&amp;O positions</td></tr>';
}

function renderOrders(rows = []) {
	const terminal = new Set(['COMPLETE', 'CANCELLED', 'REJECTED']);
	byId('orders').innerHTML = rows.length ? rows.map(order => {
		const cancellable = !terminal.has(order.status);
		return `<tr><td class="mono">${escapeHTML(order.order_id)}</td><td>${escapeHTML(order.exchange)}:${escapeHTML(order.tradingsymbol)}</td><td>${escapeHTML(order.transaction_type)}</td><td>${escapeHTML(order.order_type)}</td><td>${order.quantity} / ${order.pending_quantity}</td><td>${escapeHTML(order.status)}</td><td>${cancellable ? `<button class="secondary small" data-modify="${escapeHTML(order.order_id)}">Modify</button> <button class="danger-button small" data-cancel="${escapeHTML(order.order_id)}">Cancel</button>` : ''}</td></tr>`;
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
byId('order-exchange').addEventListener('change', () => orderPicker.clear());

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
  } catch (error) {
    output.textContent = error.payload?.decision?.message || error.message;
    output.className = 'error';
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
    const data = await api('/api/baskets', {method: 'POST', body: JSON.stringify({idempotency_key: randomID(), name: form.get('name'), order_type: orderType, legs})});
    const loss = data.result.max_loss_known ? ` Maximum planned loss: ${money(data.result.max_loss_paise)}.` : ' Maximum planned loss is unavailable for MARKET execution because fill prices are not known in advance.';
    output.textContent = data.result.message + loss;
    output.className = 'success';
  } catch (error) {
    output.textContent = error.payload?.result?.message || error.message;
    output.className = 'error';
  }
  await Promise.allSettled([refreshDashboard(), refreshAudit()]);
});

document.addEventListener('click', async event => {
  if (!event.target.closest('.instrument-picker')) document.querySelectorAll('.instrument-picker').forEach(picker => {
    picker.querySelector('.instrument-results').innerHTML = '';
    picker.querySelector('input[type="search"]').setAttribute('aria-expanded', 'false');
  });
  const cancel = event.target.dataset.cancel;
  if (cancel && confirm('Cancel this pending order?')) {
    await api(`/api/orders/${encodeURIComponent(cancel)}/cancel`, {method: 'POST', body: '{}'});
    await Promise.allSettled([refreshDashboard(), refreshAudit()]);
    return;
  }
  const token = event.target.dataset.exit;
  if (token && confirm('Exit this entire F&O position at market with automatic protection?')) {
    await api(`/api/positions/${token}/exit`, {method: 'POST', body: JSON.stringify({product: event.target.dataset.exitProduct})});
    await Promise.allSettled([refreshDashboard(), refreshAudit()]);
    return;
  }
  const modify = event.target.dataset.modify;
  if (modify) {
    const quantity = Number(prompt('New total quantity:'));
    if (!quantity) return;
    const orderType = (prompt('Order type (MARKET, LIMIT, SL, SL-M):', 'LIMIT') || '').toUpperCase();
    if (!orderType) return;
    const price = Number(prompt('Price (0 when unused):', '0'));
    const triggerPrice = Number(prompt('Trigger price (0 when unused):', '0'));
    await api(`/api/orders/${encodeURIComponent(modify)}/modify`, {method: 'POST', body: JSON.stringify({idempotency_key: randomID(), quantity, order_type: orderType, validity: 'DAY', price, trigger_price: triggerPrice})});
    await Promise.allSettled([refreshDashboard(), refreshAudit()]);
    return;
  }
});

const events = new EventSource('/api/events');
events.addEventListener('open', () => {
  byId('live-connection').textContent = 'Live';
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
});
