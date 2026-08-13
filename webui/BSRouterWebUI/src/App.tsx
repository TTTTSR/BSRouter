import { useEffect, useState } from 'react'
import { APIError, api, clearAPIKey, getAPIKey, setAPIKey } from './lib/api'
import { IconAlert, IconBox, IconCodex, IconDsh, IconDoc, IconKey, IconServer, IconTerminal, IconZcode } from './lib/icons'
import { Button, Field, Input } from './components/ui'
import Providers from './pages/Providers'
import Models from './pages/Models'
import Logs from './pages/Logs'
import Faults from './pages/Faults'
import ApiKeys from './pages/ApiKeys'
import ClaudePresets from './pages/ClaudePresets'
import CodexPresets from './pages/CodexPresets'
import ZcodePresets from './pages/ZcodePresets'
import DshPresets from './pages/DshPresets'
import type { ReactNode } from 'react'

type Page = 'providers' | 'models' | 'logs' | 'faults' | 'apikeys' | 'claudepresets' | 'codexpresets' | 'zcodepresets' | 'dshpresets'

const NAV: { key: Page; label: string; icon: ReactNode }[] = [
  { key: 'providers', label: '供应商管理', icon: <IconServer /> },
  { key: 'models', label: '模型管理', icon: <IconBox /> },
  { key: 'logs', label: '日志查看', icon: <IconDoc /> },
  { key: 'faults', label: '故障提示', icon: <IconAlert /> },
  { key: 'apikeys', label: 'API Key', icon: <IconKey /> },
  { key: 'claudepresets', label: 'Claude 预设', icon: <IconTerminal /> },
  { key: 'codexpresets', label: 'Codex 预设', icon: <IconCodex /> },
  { key: 'zcodepresets', label: 'zcode 预设', icon: <IconZcode /> },
  { key: 'dshpresets', label: 'dsh 预设', icon: <IconDsh /> },
]

function Login({ onAuthed }: { onAuthed: () => void }) {
  const [key, setKey] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit() {
    setBusy(true)
    setErr('')
    setAPIKey(key.trim())
    try {
      await api.listProviders() // 验证 key 是否有效
      onAuthed()
    } catch (e) {
      clearAPIKey()
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      <div className="login-card">
        <div className="login-brand">
          <IconKey size={20} />
          BSRouter
        </div>
        <p className="login-desc">输入网关 API Key 以接入管理端</p>
        <Field label="API Key">
          <Input
            type="password"
            value={key}
            autoFocus
            placeholder="GATEWAY_API_KEY"
            onChange={(e) => setKey(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void submit()
            }}
          />
        </Field>
        {err ? <div className="error-text">{err}</div> : null}
        <Button variant="primary" onClick={() => void submit()} disabled={busy || key.trim() === ''}>
          {busy ? '验证中…' : '接入'}
        </Button>
      </div>
    </div>
  )
}

export default function App() {
  const [authed, setAuthed] = useState(false)
  const [checking, setChecking] = useState(true)
  const [page, setPage] = useState<Page>('providers')

  // 启动探测:本地已有 key 直接进入;否则探测网关是否需要鉴权
  // (无 key 返回 200 说明网关未开启鉴权,无需登录;401 则进入登录页)。
  useEffect(() => {
    if (getAPIKey()) {
      setAuthed(true)
      setChecking(false)
      return
    }
    api.listProviders()
      .then(() => setAuthed(true))
      .catch((e: unknown) => {
        const needLogin = e instanceof APIError && e.status === 401
        setAuthed(!needLogin) // 其他错误(如网关未启动)也进入,由页面展示错误
      })
      .finally(() => setChecking(false))
  }, [])

  // 任意接口返回 401(密钥失效/被旋转)时回到登录页。
  useEffect(() => {
    const onUnauthorized = () => setAuthed(false)
    window.addEventListener('bsrouter:unauthorized', onUnauthorized)
    return () => window.removeEventListener('bsrouter:unauthorized', onUnauthorized)
  }, [])

  if (checking) {
    return (
      <div className="login-wrap">
        <span className="spinner" role="status" aria-label="loading" />
      </div>
    )
  }
  if (!authed) return <Login onAuthed={() => setAuthed(true)} />

  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="brand">
          <img src="/favicon.svg" alt="" className="brand-logo" />
          BSRouter
        </div>
        <nav className="nav">
          {NAV.map((n) => (
            <button
              key={n.key}
              type="button"
              className={`nav-item${page === n.key ? ' active' : ''}`}
              onClick={() => setPage(n.key)}
            >
              {n.icon}
              <span>{n.label}</span>
            </button>
          ))}
        </nav>
        <div className="sidebar-foot">
          <Button
            variant="ghost"
            onClick={() => {
              clearAPIKey()
              setAuthed(false)
            }}
          >
            退出接入
          </Button>
        </div>
      </aside>
      <main className="content">
        {page === 'providers' ? <Providers /> : null}
        {page === 'models' ? <Models /> : null}
        {page === 'logs' ? <Logs /> : null}
        {page === 'faults' ? <Faults /> : null}
        {page === 'apikeys' ? <ApiKeys /> : null}
        {page === 'claudepresets' ? <ClaudePresets /> : null}
        {page === 'codexpresets' ? <CodexPresets /> : null}
{page === 'zcodepresets' ? <ZcodePresets /> : null}
        {page === 'dshpresets' ? <DshPresets /> : null}
      </main>
    </div>
  )
}
