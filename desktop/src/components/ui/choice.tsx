import { Checkbox as BaseCheckbox } from '@base-ui/react/checkbox'
import { Radio as BaseRadio } from '@base-ui/react/radio'
import { RadioGroup as BaseRadioGroup } from '@base-ui/react/radio-group'
import { Check } from 'lucide-react'
import * as React from 'react'
import { cn } from '@/lib/utils'

/** A group of radios: one value chosen at a time (a single-choice question). */
export function RadioGroup<Value extends string>({ className, ...props }: React.ComponentProps<typeof BaseRadioGroup<Value>>) {
  return <BaseRadioGroup className={cn('flex flex-col gap-1', className)} {...props} />
}

/** A round radio mark; `aria-label` or a wrapping label names it. */
export function Radio({ className, ...props }: React.ComponentProps<typeof BaseRadio.Root>) {
  return (
    <BaseRadio.Root
      className={cn('grid size-4 shrink-0 place-items-center rounded-full border border-input bg-surface outline-none focus-visible:ring-2 focus-visible:ring-ring data-checked:border-primary data-disabled:opacity-50', className)}
      {...props}
    >
      <BaseRadio.Indicator className="size-2 rounded-full bg-primary" />
    </BaseRadio.Root>
  )
}

/** A square checkbox mark (a multi-select option). */
export function Checkbox({ className, ...props }: React.ComponentProps<typeof BaseCheckbox.Root>) {
  return (
    <BaseCheckbox.Root
      className={cn('grid size-4 shrink-0 place-items-center rounded-[4px] border border-input bg-surface outline-none focus-visible:ring-2 focus-visible:ring-ring data-checked:border-primary data-checked:bg-primary data-checked:text-primary-foreground data-disabled:opacity-50', className)}
      {...props}
    >
      <BaseCheckbox.Indicator>
        <Check className="size-3" />
      </BaseCheckbox.Indicator>
    </BaseCheckbox.Root>
  )
}
