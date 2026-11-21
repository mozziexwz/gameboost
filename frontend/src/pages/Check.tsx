import {Check, Overview, Result} from '../api'
import {Banner, Card, Spinner} from '../components'

// CheckState is the network check shared by the home summary and this page.
export interface CheckState {
    result: Check | null
    running: boolean
    error: string
    run: () => void
}

// canCheck reports whether a check may start: it needs a current config and
// no start or stop in progress.
export function canCheck(o: Overview, c: CheckState) {
    const busy = o.status.state === 'starting' || o.status.state === 'stopping'
    return !c.running && !busy && o.configs.some(x => x.current && !x.error)
}

export function checkLabel(c: CheckState) {
    return c.running ? '检测中' : c.result || c.error ? '重新检测' : '开始检测'
}

// shortResult is the home summary's form of a latency: the value, or why it
// failed in one word.
export function shortResult(r: Result) {
    if (r.ok) return r.text
    return r.text.includes('超时') ? '超时' : '失败'
}

export function CheckPage(props: {overview: Overview, check: CheckState}) {
    const o = props.overview
    const c = props.check
    const r = c.result
    const hasCurrent = o.configs.some(x => x.current && !x.error)
    const pending = <span className="none">{c.running ? '检测中…' : '—'}</span>
    return (
        <div className="page">
            <Card title="出口检测" actions={
                <button className="primary" onClick={c.run} disabled={!canCheck(o, c)}>
                    {c.running && <Spinner/>}{checkLabel(c)}
                </button>
            }>
                <p className="muted small">
                    通过当前节点请求 {r?.exitTarget ?? o.exitTarget}，取较快的响应。
                </p>
                {!hasCurrent && <Banner kind="warn">请先选择节点配置。</Banner>}
                {c.error && <Banner kind="error">{c.error}</Banner>}
                <dl className="check">
                    <dt>出口 IPv4</dt>
                    <dd className={r ? (r.exitIP ? 'ok' : 'bad') : ''}>
                        {r ? r.exitIP || '查询失败：' + r.exitIPError : pending}
                    </dd>
                    <dt>入口连接延迟</dt>
                    <dd className={r ? (r.entrance.ok ? 'ok' : 'bad') : ''}>
                        {r ? <>
                            {r.entrance.text}
                            {r.entranceTarget && <span className="target">{r.entranceTarget}　{r.entrance.detail}</span>}
                        </> : pending}
                    </dd>
                    <dt>出口 HTTPS 响应延迟</dt>
                    <dd className={r ? (r.exit.ok ? 'ok' : 'bad') : ''}>
                        {r ? <>
                            {r.exit.text}
                            <span className="target">经 {r.transport.toUpperCase()}　{r.exit.detail}</span>
                        </> : pending}
                    </dd>
                    <dt>节点 DNS</dt>
                    <dd className={r ? (r.dnsError ? 'bad' : 'ok') : ''}>
                        {r ? r.dnsError || '正常' : pending}
                    </dd>
                </dl>
            </Card>
        </div>
    )
}
