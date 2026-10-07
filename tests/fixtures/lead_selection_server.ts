import { join } from 'node:path';
const wt = process.argv[2];
const entry = join(wt, `apps/web/.selection-native-${process.pid}.tsx`);
await Bun.write(entry, `import React,{useState} from 'react';
import {createRoot} from 'react-dom/client';
import {flushSync} from 'react-dom';
import {DataTable} from './src/components/data-table';
const first={id:1,email:'first@example.test'},second={id:2,email:'second@example.test'},third={id:3,email:'third@example.test'};
const initial=[first,second];
window.selected=[];
const selection=rows=>window.selected=rows.map(row=>row.id);
const columns=[{accessorKey:'email',header:'Email'}];
function Probe(){const [rows,setRows]=useState(initial);window.refresh=()=>flushSync(()=>setRows([...initial]));window.replace=()=>flushSync(()=>setRows([second,third]));return <DataTable columns={columns} data={rows} enableSelection enableVirtualization={false} onSelectionChange={selection}/>;}
const root=createRoot(document.getElementById('root'));root.render(<Probe/>);
window.unmount=()=>root.unmount();
`);
let result;
try {result=await Bun.build({entrypoints:[entry],target:'browser'});} finally {await Bun.file(entry).delete();}
if(!result.success)throw new Error(String(result.logs));
const bundle=await result.outputs[0].text();
const server=Bun.serve({hostname:'127.0.0.1',port:0,fetch(req){
 if(new URL(req.url).pathname==='/bundle.js')return new Response(bundle,{headers:{'Content-Type':'application/javascript'}});
 return new Response('<style>[role=checkbox]{display:inline-block;width:20px;height:20px;border:1px solid black}</style><div id="root"></div><script type="module" src="/bundle.js"></script>',{headers:{'Content-Type':'text/html'}});
}});
console.log(JSON.stringify({port:server.port}));
process.on('SIGTERM',()=>{server.stop(true);process.exit(0);});
