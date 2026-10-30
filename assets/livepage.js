// livepage client: forwards DOM events to the server over a WebSocket and
// morphs the document with HTML pushed back by the server (via Idiomorph).
(function () {
    if (window.__livepage) {
        return
    }
    window.__livepage = true

    const script = document.currentScript
    const wsPath = (script && script.dataset.ws) || '/_livepage/ws'

    // CONNECTION

    const queue = []
    let ws = null
    let retry = 0
    let html = null
    let redrawAsked = false

    function send(payload) {
        const msg = JSON.stringify(payload)
        if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(msg)
        } else {
            queue.push(msg)
        }
    }

    function connect() {
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
        ws = new WebSocket(proto + '//' + location.host + wsPath)
        ws.onopen = function () {
            retry = 0
            while (queue.length) {
                ws.send(queue.shift())
            }
            window.dispatchEvent(new CustomEvent('livepage:open'))
        }
        ws.onmessage = function (e) {
            let msg
            try {
                msg = JSON.parse(e.data)
            } catch (err) {
                return
            }
            if (msg.type === 'html') {
                html = msg.html
                redrawAsked = false
                morph(html)
            } else if (msg.type === 'diff') {
                applyDiff(msg)
            }
        }
        ws.onclose = function () {
            ws = null
            window.dispatchEvent(new CustomEvent('livepage:close'))
            // exponential backoff, max 5s
            retry = Math.min(retry + 1, 10)
            setTimeout(connect, Math.min(250 * retry, 5000))
        }
    }

    function morph(nextHtml) {
        Idiomorph.morph(window.document, nextHtml)
        window.dispatchEvent(new CustomEvent('livepage:morph'))
    }

    // HTML diffing: server uses github.com/sergi/go-diff, this is the JS
    // equivalent. Ops are relative to the previous full HTML document.
    function fnv1a(str) {
        let hash = 0x811c9dc5
        for (const byte of new TextEncoder().encode(str)) {
            hash ^= byte
            hash = Math.imul(hash, 0x01000193)
        }
        return (hash >>> 0).toString(16)
    }

    function requestRedraw() {
        if (redrawAsked) {
            return
        }
        redrawAsked = true
        send({type: 'redraw'})
    }

    function applyDiff(msg) {
        if (html === null) {
            requestRedraw()
            return
        }

        let next = ''
        let consumed = 0
        for (const op of msg.ops || []) {
            if (op.pos > consumed) {
                next += html.slice(consumed, op.pos)
                consumed = op.pos
            }
            if (op.op === -1) {
                consumed = op.pos + (op.len || 0)
            } else if (op.op === 1) {
                next += op.text || ''
            }
        }
        if (consumed < html.length) {
            next += html.slice(consumed)
        }

        if (msg.hash && fnv1a(next) !== msg.hash) {
            requestRedraw()
            return
        }

        html = next
        redrawAsked = false
        morph(html)
    }

    window.livepage = {send: send, morph: morph}

    // DOM EVENTS

    function handlerId(type, el) {
        if (!(el instanceof Element)) {
            return ''
        }
        const match = $(el).closest("[data-livepage-" +  type + ']').get(0)
        return match ? match.getAttribute("data-livepage-" + type) || '' : ''
    }

    function forward(type, e) {
        const t = e.target
        const handler = handlerId(type, t)
        if (!handler) {
            return false
        }
        if (!handler) {
            return false
        }

        const payload = {
            type: type,
            id: t.id,
            path: domPath(t, {includeClasses: false}),
            name: t.name,
            data: t.getAttribute("data-livepage"),
            handler: handler,
        }
        if (t instanceof Element) {
            payload.value = t.value
        }
        if (type === 'keydown' || type === 'keyup' || type === 'keypress') {
            payload.key = e.key
            payload.code = e.code
        }
        if (type === 'wheel') {
            payload.deltaX = e.deltaX
            payload.deltaY = e.deltaY
        }
        if (type === 'scroll' && t instanceof Element) {
            payload.scrollTop = t.scrollTop
            payload.scrollLeft = t.scrollLeft
        }
        if (type === 'click' || type === 'dblclick' || type === 'contextmenu') {
            payload.button = e.button
            payload.clientX = e.clientX
            payload.clientY = e.clientY
        }

        send(payload)
        return true
    }

    function throttle(fn, ms) {
        let last = 0
        return function (e) {
            const now = Date.now()
            if (now - last < ms) {
                return
            }
            last = now
            fn(e)
        }
    }

    document.addEventListener('click', e => {
        forward('click', e)
    }, {capture: true})
    document.addEventListener('dblclick', e => {
        forward('dblclick', e)
    }, {capture: true})
    document.addEventListener('contextmenu', e => {
        if (forward('contextmenu', e)) {
            e.preventDefault()
        }
    }, {capture: true})
    document.addEventListener('change', e => {
        forward('change', e)
    })
    document.addEventListener('input', e => {
        forward('input', e)
    })
    document.addEventListener('keydown', e => {
        forward('keydown', e)
    }, {capture: true})
    document.addEventListener('keyup', e => {
        forward('keyup', e)
    }, {capture: true})
    document.addEventListener('focusin', e => {
        forward('focusin', e)
    }, {capture: true})
    document.addEventListener('focusout', e => {
        forward('focusout', e)
    }, {capture: true})
    document.addEventListener('scroll', throttle(e => {
        forward('scroll', e)
    }, 100), {capture: true, passive: true})
    document.addEventListener('wheel', throttle(e => {
        forward('wheel', e)
    }, 100), {capture: true, passive: true})
    document.addEventListener('submit', e => {
        if (forward('submit', e)) {
            e.preventDefault()
        }
    })

    // DOM PATH

    function formatElement(el, options = {}) {
        const {includeHref = true, includeClasses = true} = options

        const tag = el.tagName.toLowerCase()
        const id = el.id ? `#${el.id}` : ''
        const classes = includeClasses && el.classList && el.classList.length
            ? `.${Array.from(el.classList).join('.')}`
            : ''

        const attrPairs = []
        const style = window.getComputedStyle(el)
        if (style.display === 'none' || style.visibility === 'hidden' || el.hasAttribute('hidden')) {
            attrPairs.push('hidden')
        }

        if (includeHref && tag === 'a' && el.hasAttribute('href') &&
            !el.getAttribute('href').startsWith('javascript:') &&
            !el.getAttribute('href').startsWith('mailto:')) {
            attrPairs.push(`href="${el.getAttribute('href')}"`)
        }
        if (includeHref && tag === 'img' && el.hasAttribute('src') &&
            !el.getAttribute('src').startsWith('blob:') &&
            !el.getAttribute('src').startsWith('data:')) {
            attrPairs.push(`src="${el.getAttribute('src')}"`)
        }

        const attrString = attrPairs.length > 0 ? `[${attrPairs.join(',')}]` : ''
        return `${tag}${id}${classes}${attrString}`
    }

    function domPath(node, options = {}) {
        const {
            root = document.body,
            separator = ' > ',
            listOnlyWithIds = false,
            includeHref = true,
            includeClasses = true,
        } = options

        const path = []
        let current = node
        while (current && current.nodeType === Node.ELEMENT_NODE) {
            if (!listOnlyWithIds || current.id || current === node) {
                path.unshift(formatElement(current, {includeHref, includeClasses}))
            }
            if (current === root) break
            current = current.parentElement
        }
        return path.join(separator)
    }

    connect()
})()
