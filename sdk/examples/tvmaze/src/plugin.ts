const API='https://api.tvmaze.com';
function ok(body:any){return {status:200,body};}
function fetchJSON(path:string){const r=ether.http.fetch({url:API+path});if(r.status!==200)throw new Error('TVmaze returned HTTP '+r.status);return typeof r.body==='string'?JSON.parse(r.body):r.body;}
function remember(s:any){ether.kv.put('show:'+s.id,JSON.stringify({at:Date.now(),show:s}));return s;}
function shows(kind:string){let stage='settings';try{
 const max=Math.max(10,Math.min(100,Number(ether.settings.get().maxItems||30)));
 stage='date';const date=new Date().toISOString().slice(0,10), key='list:'+kind+':'+(kind==='canada'?date:max);
 stage='cache';const raw=ether.kv.get(key);if(raw){const v=JSON.parse(raw);if(Date.now()-v.at<3600000)return v.shows;}
 stage='fetch';let list:any[];
 if(kind==='canada') {const seen:any={};list=fetchJSON('/schedule?country=CA&date='+date).map((x:any)=>x.show).filter((s:any)=>s&&!seen[s.id]&&(seen[s.id]=true));}
 else {const fetched=fetchJSON('/shows?page=0');if(!Array.isArray(fetched))throw new Error('Expected array: '+JSON.stringify(fetched).slice(0,140));stage='filter';list=fetched.filter((s:any)=>s.externals?.imdb&&(s.rating?.average||0)>=7.5).sort((a:any,b:any)=>(b.rating?.average||0)-(a.rating?.average||0));}
 stage='remember';list=list.slice(0,max).map(remember);ether.kv.put(key,JSON.stringify({at:Date.now(),shows:list}));return list;
}catch(e){throw new Error(stage+': '+e.message);}
}
function resolve(id:string){if(!/^tvmaze:[0-9]+$/.test(id))throw new Error('Expected a TVmaze show identity');const n=id.slice(7),raw=ether.kv.get('show:'+n);if(raw){const c=JSON.parse(raw);if(Date.now()-c.at<86400000)return c.show;}return remember(fetchJSON('/shows/'+n));}
export function handle(req:any){
 if(req.path==='/ether/configure'){const n=Number(req.body?.maxItems??30);return ok({errors:Number.isInteger(n)&&n>=10&&n<=100?{}:{maxItems:'Choose a whole number from 10 to 100.'}});}
 if(req.path==='/ether/health')return ok({status:'ok'});
 if(req.path==='/ether/test'){const s=fetchJSON('/shows/1');return ok({ok:!!s.id,message:'TVmaze public API connected: '+s.name});}
 if(req.path.startsWith('/catalog/')){const list=shows(req.path.includes('/canada')?'canada':'acclaimed');return ok({metas:list.map((s:any)=>({id:'tvmaze:'+s.id,type:'series',name:s.name,poster:s.image?.medium,description:(s.summary||'').replace(/<[^>]*>/g,''),year:Number((s.premiered||'').slice(0,4)),imdb_id:s.externals?.imdb,tvdb_id:s.externals?.thetvdb}))});}
 if(req.path==='/ether/metadata/resolve'){const s=resolve(req.body.id);const title:any={type:'show',title:s.name,ids:{imdb:s.externals?.imdb||'',tvdb:s.externals?.thetvdb||0},summary:(s.summary||'').replace(/<[^>]*>/g,''),year:Number((s.premiered||'').slice(0,4)),genres:s.genres||[],rating:s.rating?.average||0,posterPath:s.image?.original||'',runtime:(s.averageRuntime||s.runtime||0)*60000000000,status:s.status};if(s.premiered)title.released=s.premiered+'T00:00:00Z';return ok({title});}
 if(req.path==='/ether/action/publish'){ether.collections.upsert({id:'tvmaze-acclaimed',name:'TVmaze acclaimed series',description:'A TVmaze-powered shelf. Show data: TVmaze, CC BY-SA. https://www.tvmaze.com',mediaType:'show',sources:[{kind:'plugin:community.tvmaze.catalog/acclaimed',maxItems:Number(ether.settings.get().maxItems||30)}],match:'all',rules:[],exclude:[],sort:'source',limit:100,refresh:'daily',enabled:true,targets:{collectionsTab:true,homeRow:true,users:'all'}});return ok({toast:'TVmaze acclaimed series published.',refresh:true});}
 if(req.path==='/ether/view/catalog')return ok({title:'TVmaze Native Catalog',spec:{specVersion:1,layout:'detail',header:[{kind:'page-header',title:{$:'title'},description:{$:'subtitle'}}],body:[{kind:'stat-tile',iconKey:'tv',accent:'primary',label:'$t:count',value:{$:'count'}},{kind:'stat-tile',iconKey:'database',accent:'primary',label:'$t:source',value:'$t:provider'},{kind:'panel',title:'$t:publish',blocks:[{kind:'widget',widget:'ether.actionButton',props:{label:'$t:sync',action:'publish'}}]}]},data:{title:'TVmaze Native Catalog',subtitle:'Show data from TVmaze (CC BY-SA, tvmaze.com). Supplies independent metadata plus acclaimed-series and Canadian-airing lists. Ether matches external IDs to its canonical catalogue.',count:String(shows('acclaimed').length)}});
 return {status:404,body:{error:{code:'not_found',message:'Route not found.'}}};
}
(globalThis as any).handle=handle;
