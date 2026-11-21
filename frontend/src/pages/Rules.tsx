import {useEffect, useState} from 'react'
import {api, errorText, Overview} from '../api'
import {Banner, Card} from '../components'

export function Rules(props: {overview: Overview, refresh: () => Promise<void>}) {
    const o = props.overview
    const idle = o.status.state === 'idle'
    const ip = o.mode === 'ip'
    const [ruleType, setRuleType] = useState<'processes' | 'folders'>('processes')
    const folders = !ip && ruleType === 'folders'
    const savedProcesses = o.processes.join('\n')
    const savedFolders = (o.processFolders ?? []).join('\n')
    const savedTargets = o.targets.join('\n')
    const [processText, setProcessText] = useState(savedProcesses)
    const [folderText, setFolderText] = useState(savedFolders)
    const [targetText, setTargetText] = useState(savedTargets)
    const [working, setWorking] = useState(false)
    const [message, setMessage] = useState<{kind: 'error' | 'info', text: string} | null>(null)

    useEffect(() => setProcessText(savedProcesses), [savedProcesses])
    useEffect(() => setFolderText(savedFolders), [savedFolders])
    useEffect(() => setTargetText(savedTargets), [savedTargets])

    const [saved, text, setText, saveRules] = ip ? [savedTargets, targetText, setTargetText, api.setTargets]
        : folders ? [savedFolders, folderText, setFolderText, api.setProcessFolders]
        : [savedProcesses, processText, setProcessText, api.setProcesses]
    const label = ip ? '目标 IP' : folders ? '加速文件夹' : '加速进程'

    const run = async (fn: () => Promise<void>) => {
        setMessage(null)
        setWorking(true)
        try {
            await fn()
            await props.refresh()
        } catch (e) {
            setMessage({kind: 'error', text: errorText(e)})
        } finally {
            setWorking(false)
        }
    }

    const pick = () => run(async () => {
        const paths = folders ? [await api.pickProcessFolder()].filter(Boolean) : await api.pickProcessFiles()
        if (paths.length > 0) {
            setText(current => [...new Set([...current.split('\n').filter(Boolean), ...paths])].join('\n'))
        }
    })

    const save = () => run(async () => {
        const result = await saveRules(text.split('\n'))
        setText(result.join('\n'))
        setMessage({kind: 'info', text: `已保存${label}`})
    })

    const selectRuleType = (type: 'processes' | 'folders') => {
        setMessage(null)
        setRuleType(type)
    }

    return (
        <div className="page">
            <Card title={ip ? '目标 IP' : '进程规则'} actions={
                <div className="segmented compact" role="group" aria-label="代理模式">
                    <button className={!ip ? 'on' : ''} aria-pressed={!ip} disabled={!idle || working}
                            onClick={() => run(() => api.setMode('process'))}>进程模式</button>
                    <button className={ip ? 'on' : ''} aria-pressed={ip} disabled={!idle || working}
                            onClick={() => run(() => api.setMode('ip'))}>IP 模式</button>
                </div>
            }>
                <div className="route-choice">
                    {!ip && (
                        <div className="segmented compact" role="group" aria-label="进程规则类型">
                            <button className={!folders ? 'on' : ''} aria-pressed={!folders} disabled={working}
                                    onClick={() => selectRuleType('processes')}>程序</button>
                            <button className={folders ? 'on' : ''} aria-pressed={folders} disabled={working}
                                    onClick={() => selectRuleType('folders')}>文件夹</button>
                        </div>
                    )}
                    <p className="muted">{ip ? '每行填写一个目标 IPv4、IPv6 或 CIDR 网段。'
                        : folders ? '每行填写一个文件夹完整路径，代理其中及子文件夹中的程序。'
                        : '每行填写一个进程名或可执行文件完整路径，支持 * 通配符。'}</p>
                </div>
                {message && <Banner kind={message.kind} onClose={() => setMessage(null)}>{message.text}</Banner>}
                <textarea className="process-input" aria-label={label + '清单'} rows={4} value={text}
                          onChange={e => setText(e.target.value)}
                          placeholder={ip ? '203.0.113.10\n203.0.113.0/24' : folders ? 'D:\\Games\\MyGame' : 'Game.exe'}
                          spellCheck={false} disabled={!idle || working}/>
                <div className="form-actions">
                    {!ip && <button onClick={pick} disabled={!idle || working}>{folders ? '选择文件夹…' : '选择程序…'}</button>}
                    <button className="primary" onClick={save} disabled={!idle || working || text === saved}>保存</button>
                    {!idle && <span className="muted small">停止加速后可修改分流规则。</span>}
                </div>
            </Card>
        </div>
    )
}
