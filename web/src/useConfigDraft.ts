import { useEffect, useMemo, useState } from 'react'
import type { RouterConfig } from './types'

const cloneConfig = (config: RouterConfig): RouterConfig => JSON.parse(JSON.stringify(config))

// Keeps the editable routing draft isolated from polling updates and protects
// unsaved changes from accidental navigation or reload.
export function useConfigDraft(remote?: RouterConfig) {
  const [draft, setDraft] = useState<RouterConfig | null>(null)
  const [baseline, setBaseline] = useState<RouterConfig | null>(null)

  useEffect(() => {
    if (remote && !draft) {
      setDraft(cloneConfig(remote))
      setBaseline(cloneConfig(remote))
    }
  }, [remote, draft])

  const dirty = useMemo(
    () => Boolean(draft && baseline && JSON.stringify(draft) !== JSON.stringify(baseline)),
    [draft, baseline],
  )

  useEffect(() => {
    const beforeUnload = (event: BeforeUnloadEvent) => { if (dirty) event.preventDefault() }
    window.addEventListener('beforeunload', beforeUnload)
    return () => window.removeEventListener('beforeunload', beforeUnload)
  }, [dirty])

  const acceptSaved = (config: RouterConfig) => {
    setDraft(cloneConfig(config))
    setBaseline(cloneConfig(config))
  }

  const reset = () => {
    if (baseline) setDraft(cloneConfig(baseline))
  }

  return { draft, setDraft, dirty, acceptSaved, reset }
}
