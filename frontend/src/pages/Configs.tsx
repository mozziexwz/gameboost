import {FormEvent, useState} from 'react'
import {api, errorText, Overview} from '../api'
import {Banner, Card} from '../components'

export function Configs(props: {
    overview: Overview
    refresh: () => Promise<void>
    confirm: (title: string, message: string, confirm: string, danger?: boolean) => Promise<boolean>
}) {
    const o = props.overview
    const idle = o.status.state === 'idle'
    const [message, setMessage] = useState<{kind: 'error' | 'info', text: string} | null>(null)
    const [linkForm, setLinkForm] = useState(false)
    const [name, setName] = useState('')
    const [link, setLink] = useState('')
    const [working, setWorking] = useState(false)

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

    const importNode = async (save: (overwrite: boolean) => Promise<{name: string, exists: boolean}>) => {
        let result = await save(false)
        if (result.exists) {
            if (!await props.confirm('替换配置', `已有同名配置“${result.name}”。确定替换吗？`, '替换', true)) return
            result = await save(true)
        }
        setMessage({kind: 'info', text: `已导入 ${result.name}`})
        closeLinkForm()
    }

    const importFile = () => run(async () => {
        const path = await api.pickConfigFile()
        if (path) await importNode(overwrite => api.importConfig(path, overwrite))
    })

    const importLink = (event: FormEvent<HTMLFormElement>) => {
        event.preventDefault()
        run(() => importNode(overwrite => api.importLink(name, link, overwrite)))
    }

    const closeLinkForm = () => {
        setLinkForm(false)
        setName('')
        setLink('')
    }

    const remove = (name: string) => run(async () => {
        if (!await props.confirm('删除配置', `确定从 configs 文件夹删除“${name}”吗？`, '删除', true)) return
        await api.deleteConfig(name)
    })

    return (
        <div className="page">
            <Card title="节点配置" actions={<>
                <button className="primary" disabled={working} onClick={importFile}>导入配置文件…</button>
                <button disabled={working} onClick={() => { setLinkForm(true); setMessage(null) }}>导入链接…</button>
                <button onClick={() => api.openFolder('configs')}>打开配置文件夹</button>
                <button onClick={() => props.refresh()}>刷新</button>
            </>}>
                {message && <Banner kind={message.kind} onClose={() => setMessage(null)}>{message.text}</Banner>}
                {linkForm && <form className="link-import" onSubmit={importLink}>
                    <label>配置名称<input value={name} onChange={e => setName(e.target.value)} required disabled={working}/></label>
                    <label>节点链接<input type="password" value={link} onChange={e => setLink(e.target.value)}
                                          placeholder="mierus://" autoComplete="off" required disabled={working}/></label>
                    <div className="form-actions">
                        <button className="primary" type="submit" disabled={working}>导入</button>
                        <button type="button" disabled={working} onClick={closeLinkForm}>取消</button>
                    </div>
                </form>}
                {o.configs.length === 0 && !linkForm && <p className="empty">导入节点 JSON 文件或 mierus:// 链接。</p>}
                <ul className="config-list">
                    {o.configs.map(c => (
                        <li key={c.name} className={(c.current ? 'current ' : '') + (c.error ? 'invalid' : '')}>
                            <div className="config-main">
                                <div className="config-name">
                                    {c.name.replace(/\.json$/i, '')}
                                    {c.current && <span className="badge">当前</span>}
                                </div>
                                {c.error
                                    ? <div className="bad small">无效：{c.error}</div>
                                    : <div className="muted small">入口 {c.entrance}{c.udp && ' · 支持 UDP'}</div>}
                            </div>
                            <div className="config-actions">
                                {!c.error && !c.current && (
                                    <button disabled={!idle || working} onClick={() => run(() => api.selectConfig(c.name))}>设为当前</button>
                                )}
                                <button className="danger-text" disabled={working || (!idle && c.current)} onClick={() => remove(c.name)}>删除</button>
                            </div>
                        </li>
                    ))}
                </ul>
            </Card>
        </div>
    )
}
