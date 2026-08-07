import { useCallback, useEffect, useRef, useState } from 'react'

export interface AsyncState<T> {
  data: T | null
  error: string
  loading: boolean
  reload: () => void
}

// 简单的数据加载 hook:fn 通过 ref 读取,reload() 触发重新加载。
// 仅在首次加载时置 loading(重新加载时保留旧数据,避免列表闪烁)。
export function useAsync<T>(fn: () => Promise<T>): AsyncState<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)
  const fnRef = useRef(fn)
  fnRef.current = fn
  const loadedRef = useRef(false)

  useEffect(() => {
    let cancelled = false
    if (!loadedRef.current) setLoading(true)
    setError('')
    fnRef.current()
      .then((d) => {
        if (!cancelled) setData(d)
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (!cancelled) {
          loadedRef.current = true
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [tick])

  const reload = useCallback(() => setTick((t) => t + 1), [])
  return { data, error, loading, reload }
}
