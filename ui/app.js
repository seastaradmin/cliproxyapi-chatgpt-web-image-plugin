'use strict';
const $ = (id) => document.getElementById(id);
let references = [];
let controller = null;
let resultURL = null;

function status(message, error = false) {
  $('status').textContent = message;
  $('status').dataset.error = String(error);
}

function renderReferences() {
  $('references').replaceChildren();
  $('reference-count').textContent = `${references.length} / 10`;
  references.forEach((entry, index) => {
    const card = document.createElement('div');
    card.className = 'reference';
    const image = document.createElement('img');
    image.src = entry.url;
    image.alt = `参考图 ${index + 1}`;
    const caption = document.createElement('p');
    caption.textContent = `图 ${index + 1} · ${entry.file.name}`;
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.textContent = '×';
    remove.setAttribute('aria-label', `移除参考图 ${index + 1}`);
    remove.disabled = controller !== null;
    remove.addEventListener('click', () => {
      URL.revokeObjectURL(entry.url);
      references.splice(index, 1);
      renderReferences();
    });
    card.append(image, caption, remove);
    $('references').append(card);
  });
}

$('reference-files').addEventListener('change', (event) => {
  const files = [...event.target.files];
  event.target.value = '';
  if (references.length + files.length > 10) return status('最多添加 10 张参考图。', true);
  if (files.some((file) => !['image/png', 'image/jpeg', 'image/webp'].includes(file.type))) return status('参考图仅支持 PNG、JPEG 和 WebP。', true);
  if (files.some((file) => file.size === 0 || file.size > 10 * 1024 * 1024)) return status('每张参考图须大于 0 且不超过 10 MiB。', true);
  const total = [...references.map((entry) => entry.file), ...files].reduce((sum, file) => sum + 4 * Math.ceil(file.size / 3) + 128, 0);
  if (total + new TextEncoder().encode($('prompt').value).length * 6 + 4096 > 32 * 1024 * 1024) return status('参考图总量过大，编码后的请求须小于 32 MiB。', true);
  references.push(...files.map((file) => ({ file, url: URL.createObjectURL(file) })));
  renderReferences();
  status(`已添加 ${references.length} 张参考图，按图一、图二顺序使用。`);
});

function setBusy(busy) {
  $('generate').disabled = busy;
  $('generate').textContent = busy ? '正在生成…' : '生成图片';
  $('cancel').hidden = !busy;
  for (const id of ['api-key', 'prompt', 'reference-files', 'size', 'quality']) $(id).disabled = busy;
  renderReferences();
}

function apiError(response, data) {
  const code = data?.error?.code;
  if (response.status === 401) return 'CPA API Key 无效，请使用 API 密钥管理中的 Key。';
  if (code === 'image_quota_exhausted') return '所选账号生图额度已用完，可再次生成以切换账号。';
  if (code === 'auth_unavailable' || code === 'no_free_account') return '没有可用的 free 账号，请检查 CPA 登录凭据及插件状态。';
  if (response.status === 413) return '请求过大，请减少参考图数量或文件大小。';
  if (response.status === 404 || code === 'model_not_found') return '未找到生图模型，请确认插件已启用并加载 gpt-image-2-web。';
  return `生图失败（HTTP ${response.status}）。请查看 CPA 请求日志；再次生成可能创建另一张图。`;
}

$('generation-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  if (controller) return;
  const key = $('api-key').value.trim();
  const prompt = $('prompt').value.trim();
  if (!key || !prompt) return status('请填写 CPA API Key 和画面描述。', true);
  if (new TextEncoder().encode(prompt).length > 1024 * 1024) return status('画面描述不能超过 1 MiB。', true);
  const fields = { model: 'gpt-image-2-web', prompt, n: 1, response_format: 'b64_json' };
  if ($('size').value) fields.size = $('size').value;
  if ($('quality').value) fields.quality = $('quality').value;
  let body;
  const headers = { Authorization: `Bearer ${key}` };
  const path = references.length ? '/v1/images/edits' : '/v1/images/generations';
  if (references.length) {
    const normalizedSize = references.reduce((sum, entry) => sum + 4 * Math.ceil(entry.file.size / 3) + 128, 0) + new TextEncoder().encode(JSON.stringify(fields)).length + 4096;
    if (normalizedSize > 32 * 1024 * 1024) return status('请求过大，请减少参考图数量或文件大小。', true);
    body = new FormData();
    for (const [name, value] of Object.entries(fields)) body.append(name, String(value));
    for (const entry of references) body.append('image[]', entry.file);
  } else {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(fields);
  }
  controller = new AbortController();
  setBusy(true);
  status('正在通过 ChatGPT 网页生成，通常需要几分钟，请保持页面打开。');
  try {
    const response = await fetch(path, { method: 'POST', headers, body, signal: controller.signal, credentials: 'omit', redirect: 'error', cache: 'no-store' });
    const data = await response.json().catch(() => null);
    if (!response.ok) throw new Error(apiError(response, data));
    const output = data?.data?.[0];
    const b64 = output?.b64_json || (typeof output?.url === 'string' && /^data:image\/(png|jpeg|webp);base64,/.test(output.url) ? output.url.split(',')[1] : null);
    if (!b64) throw new Error('没有收到可显示的图片，请检查 CPA 请求日志。');
    const binary = atob(b64);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    const mime = bytes[0] === 0xff && bytes[1] === 0xd8 ? 'image/jpeg' : bytes[0] === 0x52 && bytes[1] === 0x49 ? 'image/webp' : 'image/png';
    const image = $('result');
    image.onload = () => {
      $('dimensions').textContent = `${image.naturalWidth} × ${image.naturalHeight} 像素 · ${references.length ? `${references.length} 张参考图` : '文生图'}`;
      status('生成完成，可下载原始图片。');
    };
    image.onerror = () => status('收到的图片无法解码，请检查 CPA 请求日志。', true);
    if (resultURL) URL.revokeObjectURL(resultURL);
    resultURL = URL.createObjectURL(new Blob([bytes], { type: mime }));
    image.src = resultURL;
    image.hidden = false;
    $('empty').hidden = true;
    $('download').href = resultURL;
    $('download').download = `chatgpt-web-image.${mime === 'image/jpeg' ? 'jpg' : mime.split('/')[1]}`;
    $('download').hidden = false;
  } catch (error) {
    status(error.name === 'AbortError' ? '已停止等待。上游可能已开始生成，再次提交可能创建另一张图。' : error instanceof TypeError ? '连接中断，请检查 CPA 服务。上游可能已开始生成。' : error.message, true);
  } finally {
    controller = null;
    setBusy(false);
  }
});

$('cancel').addEventListener('click', () => controller?.abort());
