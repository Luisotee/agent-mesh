import { contextBridge, ipcRenderer } from 'electron'
import type { BridgeEvent, DesktopApi } from '../shared/api.js'

const listeners = new Map<string, (e: BridgeEvent) => void>()
ipcRenderer.on('bridge:event', (_e, id: string, ev: BridgeEvent) => {
  const fn = listeners.get(id)
  if (!fn) return
  if (ev.type === 'end' || ev.type === 'error') listeners.delete(id)
  fn(ev)
})

let next = 0
const api: DesktopApi = {
  hosts: {
    list: () => ipcRenderer.invoke('hosts:list'),
    add: (h) => ipcRenderer.invoke('hosts:add', h),
    remove: (id) => ipcRenderer.invoke('hosts:remove', id),
    fixed: ipcRenderer.sendSync('hosts:fixed') as boolean,
  },
  request(hostId, req, onEvent) {
    const id = String(++next)
    listeners.set(id, onEvent)
    ipcRenderer.send('bridge:request', id, hostId, req)
    return () => {
      listeners.delete(id)
      ipcRenderer.send('bridge:abort', id)
    }
  },
}
contextBridge.exposeInMainWorld('desktop', api)
