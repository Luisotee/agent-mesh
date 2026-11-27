import { useMemo, useState } from 'react'
import { Field } from '@/components/ui/label'
import { Select, Textarea } from '@/components/ui/input'
import { useHostState } from '@/context'
import { RUN_LEVEL_LABEL, askBeforeOf, runAutonomyOf, type RunLevel } from '@/autonomy'
import type { HostRuntime } from '@/model'

/** What a new run's lead may do on its own: the host's settings, or a level for this run (`StartRun`'s autonomy), and what it asks first. */
export function useRunAutonomy() {
  const [level, setLevel] = useState<RunLevel>('inherit')
  const [ask, setAsk] = useState('')
  return { level, setLevel, ask, setAsk, value: () => runAutonomyOf(level, ask) }
}

export function RunAutonomyFields({ host, state }: { host: HostRuntime; state: ReturnType<typeof useRunAutonomy> }) {
  const settings = useHostState(host, (s) => s.settings)
  const always = useMemo(() => askBeforeOf(settings), [settings])
  return (
    <>
      <Field label="What the lead may do on its own" hint="Only for this run. Hold single PRs in the side panel.">
        <Select value={state.level} onChange={(e) => state.setLevel(e.target.value as RunLevel)}>
          {(Object.keys(RUN_LEVEL_LABEL) as RunLevel[]).map((l) => <option key={l} value={l}>{RUN_LEVEL_LABEL[l]}</option>)}
        </Select>
      </Field>
      <Field label="Ask me before (one per line)" hint={always.length > 0 ? `Always asked, from the host's settings: ${always.join('; ')}.` : 'The lead asks you before doing any of these, in this run.'}>
        <Textarea value={state.ask} onChange={(e) => state.setAsk(e.target.value)} placeholder="deploys&#10;migrations" className="min-h-14" />
      </Field>
    </>
  )
}
