import { join } from "node:path";
const wt = process.argv[2];
const entry = join(wt, `apps/web/.clipboard-native-${process.pid}.tsx`);
await Bun.write(entry, `import React from 'react';
import {createRoot} from 'react-dom/client';
import {QueryClient,QueryClientProvider} from '@tanstack/react-query';
import {MemoryRouter,Routes,Route} from 'react-router-dom';
import Editor from './src/pages/workbook-editor';
import {QuickLookProvider} from './src/components/quick-look/quick-look';
const root=createRoot(document.getElementById('root'));
root.render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><QuickLookProvider><MemoryRouter initialEntries={['/workbooks/native']}><Routes><Route path="/workbooks/:id" element={<Editor/>}/></Routes></MemoryRouter></QuickLookProvider></QueryClientProvider>);
window.unmount=()=>root.unmount();`);
let result;
try {result=await Bun.build({entrypoints:[entry],target:"browser"});} finally {await Bun.file(entry).delete();}
if(!result.success)throw new Error(String(result.logs));
const bundle=await result.outputs.find(output=>output.type.includes("javascript"))!.text();
const columns=[{id:"notes",name:"Notes",type:"input",width:240},{id:"company",name:"Company",type:"input",width:240}];
const rows=[1,2,3].map(id=>({row_id:id,lead_id:null,data:{notes:id===1?'first\nsecond':'Keep notes '+id,company:'Keep company '+id},lead:{},enrichments:{}}));
const updates:unknown[]=[];
const server=Bun.serve({hostname:"127.0.0.1",port:0,fetch(req,srv){
 const path=new URL(req.url).pathname;
 if(path==="/bundle.js")return new Response(bundle,{headers:{"Content-Type":"application/javascript"}});
 if(path.endsWith('/ws')){if(srv.upgrade(req))return;return new Response('upgrade failed',{status:400});}
 if(path==="/received")return Response.json(updates);
 if(path==="/api/workbooks/native/rows" && req.method==="PATCH")return req.json().then(body=>{updates.push(body);return Response.json({status:"updated",updated_rows:body.updates.length,reactive_columns:[]});});
 if(path==="/api/workbooks/native")return Response.json({workbook:{id:"native",name:"Clipboard",status:"draft",source_type:"manual",source_config:{},filter_criteria:{},columns_config:columns,total_rows:3,completed_rows:0,sync_to_leads:false},rows,total_rows:3,query_total_rows:3,page:1,page_size:1000,has_more:false});
 if(path.endsWith('/views'))return Response.json({views:[],total:0});
 if(path.endsWith('/connector-runs'))return Response.json({runs:[],next_cursor:null,has_more:false});
 if(path.endsWith('/runs'))return Response.json({runs:[],total:0});
 if(path.endsWith('/providers'))return Response.json({providers:[]});
 if(path.endsWith('/cost'))return Response.json({budget_spent_usd:0,budget_max_usd:0});
 if(path.endsWith('/estimate'))return Response.json({best_usd:0,worst_usd:0,rows:3});
 if(path.includes('/api/'))return Response.json({presets:[]});
 return new Response('<style>body{margin:0}table{display:table}td{width:240px;height:40px}#root{height:800px}[class*=overflow]{height:600px}</style><div id="root"></div><script type="module" src="/bundle.js"></script>',{headers:{"Content-Type":"text/html"}});
},websocket:{message(){}}});
console.log(JSON.stringify({port:server.port}));
process.on("SIGTERM",()=>{server.stop(true);process.exit(0);});
