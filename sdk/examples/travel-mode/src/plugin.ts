export function rank_key(c:any,cfg:any={}):number{
 const ceiling=Math.max(480,Math.min(2160,Number(cfg.maxHeight)||1080));
 const budget=Math.max(1,Number(cfg.value??cfg.maxGB)||4)*1073741824;
 const height=parseInt(String(c.resolution||'0').replace(/[^0-9]/g,''),10)||0;
 const clientMax=Number(c.clientCaps?.maxHeight)||ceiling;
 let score=c.cached?300:0;
 if(height>Math.min(ceiling,clientMax))score-=10000;else if(height>0)score+=Math.min(height,1080);
 if(c.size>budget)score-=2000+Math.min(10000,Math.floor((c.size-budget)/1073741824)*100);else if(c.size>0)score+=Math.max(0,500-Math.floor(c.size/1073741824)*100);
 const codec=String(c.videoCodec||'').toLowerCase();const supports=(c.clientCaps?.codecs||[]).map((x:string)=>x.toLowerCase());
 if(codec&&supports.length&&!supports.includes(codec))score-=20000;
 if((c.languages||[]).some((x:string)=>x.toLowerCase()===(cfg.language||'en').toLowerCase()))score+=100;
 return Math.round(score);
}
const SAMPLES=[{name:'1080p cached · 2 GB',resolution:'1080p',size:2*1073741824,cached:true,languages:['en'],videoCodec:'h264'},{name:'720p cached · 1 GB',resolution:'720p',size:1073741824,cached:true,languages:['en'],videoCodec:'h264'},{name:'4K remux · 55 GB',resolution:'2160p',size:55*1073741824,cached:true,languages:['en'],videoCodec:'hevc'}];
export function handle(req:any){
 if(req.path==='/ether/configure')return {status:200,body:{errors:{}}};
 if(req.path==='/ether/health')return {status:200,body:{status:'ok'}};
 if(req.path==='/ether/view/scorecard')return {status:200,body:{title:'Travel Mode',spec:{specVersion:1,layout:'detail',header:[{kind:'page-header',title:{$:'title'},description:{$:'subtitle'}}],body:[{kind:'panel',title:'$t:samples',blocks:[{kind:'text',content:'$t:sample_hint'},{kind:'repeat',items:{$:'scores'},itemKey:{$:'name'},blocks:[{kind:'stat-tile',iconKey:'gauge',accent:'primary',label:{$:'name'},value:{$:'score'}}]}]}]},data:{title:'Travel Mode',subtitle:'Adds a ranking step in Quality with a selectable file-size target. Defaults: 1080p, 4 GB and English audio. This scorecard shows the default settings.',scores:SAMPLES.map(c=>({name:c.name,score:String(rank_key(c))}))}}};
 return {status:404,body:{error:{code:'not_found',message:'Route not found.'}}};
}
(globalThis as any).handle=handle;(globalThis as any).rank_key=rank_key;
