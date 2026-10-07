import { join } from 'node:path';
const wt = process.argv[2];
const entry = join(wt, `apps/web/.socket-native-${process.pid}.tsx`);
await Bun.write(entry, `import React from 'react';
import {createRoot} from 'react-dom/client';
import {flushSync} from 'react-dom';
import {QueryClient,QueryClientProvider} from '@tanstack/react-query';
import {useWorkbookSocket,workbookKeys} from './src/lib/workbook-hooks';
const qc=new QueryClient();
const root=createRoot(document.getElementById('root'));
window.acks=0;
const NativeSocket=window.WebSocket;
window.WebSocket=class extends NativeSocket {constructor(...args){super(...args);this.addEventListener('message',event=>{if(JSON.parse(event.data).colId==='ack')window.acks++;});}};
const frames=new Map();let serial=0;
window.requestAnimationFrame=cb=>{frames.set(++serial,cb);return serial;};
window.cancelAnimationFrame=id=>frames.delete(id);
function Probe({id}){useWorkbookSocket(id);return <div>{id}</div>}
window.seed=id=>qc.setQueryData(workbookKeys.detail(id),{workbook:{id},rows:[{lead_id:7,lead:{company:'Acme'},enrichments:{}}]});
window.mount=id=>flushSync(()=>root.render(<QueryClientProvider client={qc}><Probe id={id}/></QueryClientProvider>));
window.pending=()=>frames.size;
window.flush=()=>{const work=[...frames.values()];frames.clear();work.forEach(cb=>cb(performance.now()));};
window.snapshot=id=>qc.getQueryData(workbookKeys.detail(id));
window.unmount=()=>root.unmount();
`);
let result;
try {result=await Bun.build({entrypoints:[entry],target:'browser'});} finally {await Bun.file(entry).delete();}
if(!result.success)throw new Error(String(result.logs));
const bundle=await result.outputs[0].text();
const sockets=new Map();
const server=Bun.serve({hostname:'127.0.0.1',port:0,fetch(req,s){
 const url=new URL(req.url);
 if(url.pathname.endsWith('/ws')){s.upgrade(req,{data:{id:url.pathname.split('/')[3]}});return;}
 if(url.pathname==='/connected')return Response.json([...sockets.keys()]);
 if(url.pathname==='/send'){const ws=sockets.get(url.searchParams.get('id'));if(!ws)return new Response('not connected',{status:409});ws.send(JSON.stringify({type:'cell_update',leadId:7,colId:url.searchParams.get('column'),value:url.searchParams.get('value'),status:'complete'}));return new Response('sent');}
 if(url.pathname==='/bundle.js')return new Response(bundle,{headers:{'Content-Type':'application/javascript'}});
 return new Response('<div id="root"></div><script src="/bundle.js"></script>',{headers:{'Content-Type':'text/html'}});
},websocket:{open(ws){sockets.set(ws.data.id,ws);},close(ws){if(sockets.get(ws.data.id)===ws)sockets.delete(ws.data.id);}}});
console.log(JSON.stringify({port:server.port}));
process.on('SIGTERM',()=>{server.stop(true);process.exit(0);});
