import type { ReactNode } from 'react'

// 极简线条图标(SVG,黑白灰,无 emoji)。stroke 使用 currentColor 继承文本色。

interface IconBase { size?: number }

function S({ size = 18, children }: IconBase & { children: ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {children}
    </svg>
  )
}

// 供应商(服务器机架)
export function IconServer({ size }: IconBase) {
  return (
    <S size={size}>
      <rect x="3" y="4" width="18" height="6" />
      <rect x="3" y="14" width="18" height="6" />
      <line x1="7" y1="7" x2="7.01" y2="7" />
      <line x1="7" y1="17" x2="7.01" y2="17" />
      <line x1="11" y1="7" x2="17" y2="7" />
      <line x1="11" y1="17" x2="17" y2="17" />
    </S>
  )
}

// 模型(立方体)
export function IconBox({ size }: IconBase) {
  return (
    <S size={size}>
      <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
      <polyline points="3.27 6.96 12 12.01 20.73 6.96" />
      <line x1="12" y1="22.08" x2="12" y2="12" />
    </S>
  )
}

// 日志(文档)
export function IconDoc({ size }: IconBase) {
  return (
    <S size={size}>
      <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <polyline points="14 2 14 8 20 8" />
      <line x1="8" y1="13" x2="16" y2="13" />
      <line x1="8" y1="17" x2="13" y2="17" />
    </S>
  )
}

export function IconPlus({ size }: IconBase) {
  return (
    <S size={size}>
      <line x1="12" y1="5" x2="12" y2="19" />
      <line x1="5" y1="12" x2="19" y2="12" />
    </S>
  )
}

export function IconRefresh({ size }: IconBase) {
  return (
    <S size={size}>
      <polyline points="23 4 23 10 17 10" />
      <path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10" />
    </S>
  )
}

export function IconTrash({ size }: IconBase) {
  return (
    <S size={size}>
      <polyline points="3 6 5 6 21 6" />
      <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
    </S>
  )
}

export function IconEdit({ size }: IconBase) {
  return (
    <S size={size}>
      <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
      <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
    </S>
  )
}

export function IconPing({ size }: IconBase) {
  return (
    <S size={size}>
      <polygon points="22 2 15 22 11 13 2 9 22 2" />
      <line x1="22" y1="2" x2="11" y2="13" />
    </S>
  )
}

export function IconX({ size }: IconBase) {
  return (
    <S size={size}>
      <line x1="18" y1="6" x2="6" y2="18" />
      <line x1="6" y1="6" x2="18" y2="18" />
    </S>
  )
}

export function IconChevron({ size }: IconBase) {
  return (
    <S size={size}>
      <polyline points="6 9 12 15 18 9" />
    </S>
  )
}

export function IconKey({ size }: IconBase) {
  return (
    <S size={size}>
      <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
    </S>
  )
}

export function IconActivity({ size }: IconBase) {
  return (
    <S size={size}>
      <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />
    </S>
  )
}

// Claude 预设(终端提示符)
export function IconTerminal({ size }: IconBase) {
  return (
    <S size={size}>
      <polyline points="4 17 10 11 4 5" />
      <line x1="12" y1="19" x2="20" y2="19" />
    </S>
  )
}
