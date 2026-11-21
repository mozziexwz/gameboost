// Typed access to the Go backend. The generated Wails bindings return class
// instances; they are used here as plain data with the shapes below.
import * as Go from '../wailsjs/go/main/App'
import {EventsOn} from '../wailsjs/runtime/runtime'

export type State = 'idle' | 'starting' | 'running' | 'stopping'

// How the core reaches the node. UDP needs a configuration with a UDP binding.
export type Transport = 'tcp' | 'udp'
export type RouteMode = 'process' | 'ip'

export interface Status {
    state: State
    config: string
    sinceUnix: number
    upload: number // bytes exchanged with the node
    download: number
    uploadRate: number // bytes per second
    downloadRate: number
    systemProxy: string[] | null // system proxy endpoints being taken over
    transport: Transport | '' // in use while accelerating
    lastError: string
}

export interface ConfigItem {
    name: string
    profile: string
    entrance: string
    udp: boolean // the node also serves UDP
    error: string
    current: boolean
}

export interface Conflict {
    message: string
    blocking: boolean
    fix: string // a fix fixConflict can apply, or ""
}

export interface Overview {
    status: Status
    configs: ConfigItem[]
    mode: RouteMode
    processes: string[]
    processFolders: string[] | null
    targets: string[]
    exitTarget: string
    transport: Transport // the user's choice
    elevated: boolean
    conflicts: Conflict[]
    lockError: string
    settingsError: string
    dataDir: string
    version: string
}

export interface Result {
    text: string
    detail: string
    ok: boolean
    averageMs: number
}

export interface Check {
    exitIP: string
    exitIPError: string
    entranceTarget: string
    entrance: Result
    exitTarget: string
    exit: Result
    dnsError: string
    transport: Transport
}

export interface LogLine {
    seq: number
    time: string
    tag: string
    text: string
}

export interface Component {
    name: string
    version: string
    copyright: string
    license: string
    url: string
}

export interface Link {
    name: string
    url: string
}

export interface Notice {
    kind: 'error' | 'warn' | 'info'
    text: string
}

export interface About {
    version: string
    copyright: string
    license: string
    components: Component[]
    licenseTexts: Link[]
}

const plain = <T>(v: unknown) => v as T

export const api = {
    overview: async () => plain<Overview>(await Go.Overview()),
    start: async () => (await Go.Start()) ?? [],
    stop: () => Go.Stop(),
    runningProcesses: async () => (await Go.RunningProcesses()) ?? [],
    pickProcessFiles: async () => (await Go.PickProcessFiles()) ?? [],
    pickProcessFolder: () => Go.PickProcessFolder(),
    setProcesses: (names: string[]) => Go.SetProcesses(names),
    setProcessFolders: async (paths: string[]) => (await Go.SetProcessFolders(paths)) ?? [],
    setMode: (mode: RouteMode) => Go.SetMode(mode),
    setTargets: (targets: string[]) => Go.SetTargets(targets),
    selectConfig: (name: string) => Go.SelectConfig(name),
    pickConfigFile: () => Go.PickConfigFile(),
    importConfig: async (path: string, overwrite: boolean) =>
        plain<{name: string, exists: boolean}>(await Go.ImportConfig(path, overwrite)),
    importLink: async (name: string, link: string, overwrite: boolean) =>
        plain<{name: string, exists: boolean}>(await Go.ImportLink(name, link, overwrite)),
    deleteConfig: (name: string) => Go.DeleteConfig(name),
    openFolder: (kind: 'configs' | 'logs' | 'data') => Go.OpenFolder(kind),
    setTransport: (transport: Transport) => Go.SetTransport(transport),
    runCheck: async () => plain<Check>(await Go.RunCheck()),
    recentLogs: async () => plain<LogLine[]>(await Go.RecentLogs()) ?? [],
    exportDiagnostics: () => Go.ExportDiagnostics(),
    cleanup: async () => (await Go.Cleanup()) ?? [],
    fixConflict: async (fix: string) => (await Go.FixConflict(fix)) ?? [],
    about: async () => plain<About>(await Go.About()),
    openURL: (url: string) => Go.OpenURL(url),
    hideToTray: () => Go.HideToTray(),
    quit: () => Go.Quit(),
}

export const on = {
    status: (fn: (s: Status) => void) => EventsOn('status', fn),
    logs: (fn: (lines: LogLine[]) => void) => EventsOn('logs', fn),
    notice: (fn: (n: Notice) => void) => EventsOn('notice', fn),
}

// errorText turns a rejected binding call into a readable message.
export function errorText(err: unknown): string {
    if (typeof err === 'string') return err
    if (err instanceof Error) return err.message
    return String(err)
}
