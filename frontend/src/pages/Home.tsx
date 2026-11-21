import {useState} from 'react'
import {api, errorText, Overview, RouteMode, Transport} from '../api'
import {Banner, Spinner} from '../components'
import {canCheck, CheckState, checkLabel, shortResult} from './Check'

const fixLabels: Record<string, string> = {
    proxybridge: '停止 ProxyBridge',
}

const stateText = {
    idle: '未加速',
    starting: '正在开始加速…',
    running: '加速中',
    stopping: '正在停止加速…',
}

// CheckSummary is the one-line form of the network check on the home page.
function CheckSummary(props: {check: CheckState}) {
    const {result: r, running, error} = props.check
    if (running) return <span className="summary none"><Spinner/>正在检测出口 IPv4 和延迟…</span>
    if (error) return <span className="summary"><b className="bad">检测失败</b></span>
    if (!r) return <span className="summary none">IPv4 —　入口 —　目标 —</span>
    return (
        <span className="summary">
            <span>IPv4 <b className={r.exitIP ? '' : 'bad'}>{r.exitIP || '查询失败'}</b></span>
            <span>入口 <b className={r.entrance.ok ? 'ok' : 'bad'}>{shortResult(r.entrance)}</b></span>
            <span>目标 <b className={r.exit.ok ? 'ok' : 'bad'}>{shortResult(r.exit)}</b></span>
        </span>
    )
}

export function Home(props: {
    overview: Overview
    refresh: () => Promise<void>
    confirm: (title: string, message: string, confirm: string, danger?: boolean) => Promise<boolean>
    check: CheckState
    goConfigs: () => void
    goCheck: () => void
    goRules: () => void
}) {
    const o = props.overview
    const st = o.status
    const busy = st.state === 'starting' || st.state === 'stopping'
    const running = st.state === 'running'
    const validConfigs = o.configs.filter(c => !c.error)
    const blocking = o.conflicts.filter(c => c.blocking)
    const [warnings, setWarnings] = useState<string[]>([])
    const [error, setError] = useState('')
    const [fixing, setFixing] = useState('')
    const [fixReport, setFixReport] = useState<string[]>([])

    const current = validConfigs.find(c => c.current)
    const routeCount = o.mode === 'ip' ? o.targets.length : o.processes.length + (o.processFolders?.length ?? 0)
    const canStart = !busy && o.elevated && !o.lockError && !!current && routeCount > 0 && blocking.length === 0
    // Only a node that serves UDP offers the choice. While accelerating the
    // control shows what is in use, which is TCP after a UDP fallback.
    const shownTransport = st.state === 'idle' || !st.transport ? o.transport : st.transport

    const toggle = async () => {
        setError('')
        try {
            if (running) {
                const processes = await api.runningProcesses()
                if (o.mode === 'ip' || processes.length > 0) {
                    const note = processes.length ? `正在运行 ${processes.join('、')}。` : ''
                    if (!await props.confirm('停止加速', note + '停止加速会断开代理连接。确定停止吗？', '停止加速', true)) return
                }
                await api.stop()
            } else {
                setWarnings(await api.start())
            }
        } catch (e) {
            setError(errorText(e))
        } finally {
            await props.refresh()
        }
    }

    const select = async (name: string) => {
        setError('')
        try {
            await api.selectConfig(name)
        } catch (e) {
            setError(errorText(e))
        }
        await props.refresh()
    }

    const chooseTransport = async (t: Transport) => {
        setError('')
        try {
            await api.setTransport(t)
        } catch (e) {
            setError(errorText(e))
        }
        await props.refresh()
    }

    const chooseMode = async (mode: RouteMode) => {
        setError('')
        try {
            await api.setMode(mode)
        } catch (e) {
            setError(errorText(e))
        }
        await props.refresh()
    }

    const fixConflict = async (fix: string) => {
        if (!await props.confirm('停止 ProxyBridge',
            'GAMEBOOST 会退出 ProxyBridge 并停止其后台服务（ProxyBridgeSvc）。确定继续吗？', '停止', true)) {
            return
        }
        setFixing(fix)
        setError('')
        setFixReport([])
        try {
            setFixReport(await api.fixConflict(fix))
        } catch (e) {
            setError(errorText(e))
        } finally {
            setFixing('')
            await props.refresh()
        }
    }

    // One button per fix, even when two conflicts share it.
    const fixShown = new Set<string>()
    const fixAction = (fix: string) => {
        if (!fix || fixShown.has(fix) || busy) return undefined
        fixShown.add(fix)
        return {label: fixLabels[fix] ?? '自动处理', onClick: () => fixConflict(fix), busy: fixing === fix}
    }

    const note = running ? [
            st.transport ? `经 ${st.transport.toUpperCase()} 连接节点` : '',
            st.systemProxy && st.systemProxy.length > 0
                ? `已接管加速进程的系统代理连接（${st.systemProxy.join('、')}）` : '',
        ].filter(Boolean).join(' · ')
        : st.state === 'starting' ? '正在加载分流组件…'
        : st.state === 'stopping' ? '正在断开连接并卸载分流驱动…'
        : !current ? '请先选择节点配置'
        : routeCount === 0 ? (o.mode === 'ip' ? '请先添加并保存目标 IP' : '请先添加并保存加速进程或文件夹')
        : '开始加速后再打开对应程序'

    return (
        <div className="page home">
            {o.lockError && <Banner kind="error">{o.lockError}</Banner>}
            {!o.elevated && <Banner kind="error">程序没有以管理员身份运行，无法开始加速。请右键程序选择“以管理员身份运行”。</Banner>}
            {o.settingsError && <Banner kind="warn">{o.settingsError}</Banner>}
            {o.conflicts.map(c => (
                <Banner key={c.message} kind={c.blocking ? 'error' : 'warn'} action={fixAction(c.fix)}>{c.message}</Banner>
            ))}
            {fixReport.length > 0 && <Banner kind="info" onClose={() => setFixReport([])}>{fixReport.join('；')}</Banner>}
            {error && <Banner kind="error" onClose={() => setError('')}>{error}</Banner>}
            {warnings.map(w => <Banner key={w} kind="warn" onClose={() => setWarnings(ws => ws.filter(x => x !== w))}>{w}</Banner>)}
            <section className="lines">
                <div className="line">
                    <span className="line-label">节点配置</span>
                    {validConfigs.length === 0
                        ? <span className="muted">还没有可用的节点配置。</span>
                        : (
                            <select aria-label="当前节点" value={current?.name ?? ''} disabled={st.state !== 'idle'}
                                    title={st.state !== 'idle' ? '加速中不能切换配置，请先停止加速' : undefined}
                                    onChange={e => select(e.target.value)}>
                                {!current && <option value="" disabled>请选择节点配置</option>}
                                {validConfigs.map(c => <option key={c.name} value={c.name}>{c.name.replace(/\.json$/i, '')}</option>)}
                            </select>
                        )}
                    <span className="line-actions">
                        {current?.udp && (
                            <div className="segmented compact" role="group" aria-label="连接方式"
                                 title={st.state !== 'idle' ? '加速中不能切换连接方式，请先停止加速' : 'UDP 连不上节点时会自动改用 TCP'}>
                                {(['tcp', 'udp'] as Transport[]).map(t => (
                                    <button key={t} className={t === shownTransport ? 'on' : ''} disabled={st.state !== 'idle'}
                                            onClick={() => t !== o.transport && chooseTransport(t)}>{t.toUpperCase()}</button>
                                ))}
                            </div>
                        )}
                        <button className="link" onClick={props.goConfigs}>管理</button>
                    </span>
                </div>
                <div className="line">
                    <span className="line-label">代理模式</span>
                    <div className="route-choice">
                        <div className="segmented compact" role="group" aria-label="代理模式">
                            <button className={o.mode === 'process' ? 'on' : ''} aria-pressed={o.mode === 'process'}
                                    disabled={st.state !== 'idle'} onClick={() => chooseMode('process')}>进程模式</button>
                            <button className={o.mode === 'ip' ? 'on' : ''} aria-pressed={o.mode === 'ip'}
                                    disabled={st.state !== 'idle'} onClick={() => chooseMode('ip')}>IP 模式</button>
                        </div>
                        <span className="muted small">已配置 {routeCount} 项</span>
                    </div>
                    <button className="link" onClick={props.goRules}>管理</button>
                </div>
                <div className="line">
                    <span className="line-label">出口检测</span>
                    <CheckSummary check={props.check}/>
                    <span className="line-actions">
                        <button className="small" onClick={props.check.run} disabled={!canCheck(o, props.check)}>{checkLabel(props.check)}</button>
                        <button className="link" onClick={props.goCheck}>详情</button>
                    </span>
                </div>
            </section>

            <section className="home-foot">
                <div className="state-block">
                    <div className={'state-title ' + st.state}><span className="dot"/>{stateText[st.state]}</div>
                    {note && <div className="state-note">{note}</div>}
                </div>
                <button className={'hero-button ' + (running ? 'stop' : 'start')} disabled={running ? busy : !canStart} onClick={toggle}>
                    {busy && <Spinner/>}
                    {running ? '停止加速' : st.state === 'starting' ? '正在开始…' : st.state === 'stopping' ? '正在停止…' : '开始加速'}
                </button>
            </section>
        </div>
    )
}
