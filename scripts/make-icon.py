from PIL import Image, ImageDraw, ImageFilter
import numpy as np, os, math
S=2; W=1024*S
img=Image.new("RGBA",(W,W),(0,0,0,0))
margin=int(64*S); size=W-margin*2; radius=int(200*S)
# bg
top=np.array([0x1E,0x27,0x6B],dtype=np.float32); bot=np.array([0x6A,0x59,0xE8],dtype=np.float32)
yy,xx=np.mgrid[0:W,0:W]; t=np.clip((xx*0.55+yy*0.75)/(W*1.3),0,1)
grad=(top[None,None,:]*(1-t)[...,None]+bot[None,None,:]*t[...,None]).astype(np.uint8)
gimg=Image.fromarray(grad,"RGB").convert("RGBA")
mask=Image.new("L",(W,W),0); ImageDraw.Draw(mask).rounded_rectangle([margin,margin,margin+size,margin+size],radius=radius,fill=255)
img.paste(gimg,(0,0),mask)
light=Image.new("RGBA",(W,W),(0,0,0,0)); ImageDraw.Draw(light).ellipse([int(0.05*W),int(0.01*W),int(0.60*W),int(0.50*W)],fill=(150,170,255,50))
light=light.filter(ImageFilter.GaussianBlur(80*S)); img=Image.alpha_composite(img,Image.composite(light,Image.new("RGBA",(W,W),(0,0,0,0)),mask))
rim=Image.new("RGBA",(W,W),(0,0,0,0)); ImageDraw.Draw(rim).rounded_rectangle([margin+4*S,margin+4*S,margin+size-4*S,margin+size-4*S],radius=radius-4*S,outline=(255,255,255,36),width=3*S); img=Image.alpha_composite(img,rim)

cx=int(0.50*W); body_w,body_h=452*S,302*S; body_top=int(0.515*W); body_r=56*S
body_box=[cx-body_w//2,body_top,cx+body_w//2,body_top+body_h]
# shadow
shadow=Image.new("RGBA",(W,W),(0,0,0,0)); ImageDraw.Draw(shadow).rounded_rectangle([body_box[0]-6*S,body_box[1]+28*S,body_box[2]+6*S,body_box[3]+46*S],radius=body_r+12*S,fill=(8,10,34,140)); shadow=shadow.filter(ImageFilter.GaussianBlur(30*S)); img=Image.alpha_composite(img,shadow)
# body
body_mask=Image.new("L",(W,W),0); ImageDraw.Draw(body_mask).rounded_rectangle(body_box,radius=body_r,fill=255)
bt=np.array([255,255,255],dtype=np.float32); bb=np.array([0xE4,0xE9,0xFF],dtype=np.float32)
tt=np.clip((yy-body_box[1])/body_h,0,1); bgc=(bt[None,None,:]*(1-tt)[...,None]+bb[None,None,:]*tt[...,None]).astype(np.uint8)
body_img=Image.fromarray(bgc,"RGB").convert("RGBA"); body_img.putalpha(body_mask); img=Image.alpha_composite(img,body_img)
# inner rim
inner=Image.new("RGBA",(W,W),(0,0,0,0)); ImageDraw.Draw(inner).rounded_rectangle([body_box[0]+10*S,body_box[1]+10*S,body_box[2]-10*S,body_box[3]-10*S],radius=body_r-10*S,outline=(255,255,255,100),width=3*S); inner=Image.composite(inner,Image.new("RGBA",(W,W),(0,0,0,0)),body_mask); img=Image.alpha_composite(img,inner)
# container ribs
ribs=Image.new("RGBA",(W,W),(0,0,0,0)); rd=ImageDraw.Draw(ribs)
for dx in [-int(body_w*0.36), int(body_w*0.36)]:
    x=cx+dx; rd.rounded_rectangle([x-5*S,body_top+58*S,x+5*S,body_top+body_h-62*S],radius=5*S,fill=(0xC6,0xD0,0xF7,120))
ribs=Image.composite(ribs,Image.new("RGBA",(W,W),(0,0,0,0)),body_mask); img=Image.alpha_composite(img,ribs)
# lid
lid_w=506*S; lid_h=94*S; lid_y=body_top-int(lid_h*0.72); lid_box=[cx-lid_w//2,lid_y,cx+lid_w//2,lid_y+lid_h]
lid_mask=Image.new("L",(W,W),0); ImageDraw.Draw(lid_mask).rounded_rectangle(lid_box,radius=40*S,fill=255)
lt=np.array([0xF6,0xF8,0xFF],dtype=np.float32); lb=np.array([0xD7,0xDF,0xFF],dtype=np.float32)
tt2=np.clip((yy-lid_y)/lid_h,0,1); lg=(lt[None,None,:]*(1-tt2)[...,None]+lb[None,None,:]*tt2[...,None]).astype(np.uint8)
lid_img=Image.fromarray(lg,"RGB").convert("RGBA"); lid_img.putalpha(lid_mask)
# light lid shadow only 10px, very soft
ls=Image.new("RGBA",(W,W),(0,0,0,0)); ImageDraw.Draw(ls).rounded_rectangle([lid_box[0]+10*S,lid_y+lid_h-6*S,lid_box[2]-10*S,lid_y+lid_h+8*S],radius=8*S,fill=(60,70,140,40)); ls=ls.filter(ImageFilter.GaussianBlur(6*S)); img=Image.alpha_composite(img,ls)
img=Image.alpha_composite(img,lid_img)
shine=Image.new("RGBA",(W,W),(0,0,0,0)); ImageDraw.Draw(shine).rounded_rectangle([lid_box[0]+16*S,lid_y+10*S,lid_box[2]-16*S,lid_y+int(lid_h*0.5)],radius=26*S,fill=(255,255,255,100)); shine=Image.composite(shine,Image.new("RGBA",(W,W),(0,0,0,0)),lid_mask); img=Image.alpha_composite(img,shine)
# bottom label
lab=Image.new("RGBA",(W,W),(0,0,0,0)); ImageDraw.Draw(lab).rounded_rectangle([body_box[0]+70*S,body_box[3]-52*S,body_box[2]-70*S,body_box[3]-44*S],radius=4*S,fill=(0xBE,0xC9,0xF6,150)); lab=Image.composite(lab,Image.new("RGBA",(W,W),(0,0,0,0)),body_mask); img=Image.alpha_composite(img,lab)
# moon
moon_r=130*S; mx=int(cx+168*S); my=int(lid_y+4*S)
dist=np.sqrt((xx-mx)**2+(yy-my)**2); ga=np.clip(1-dist/(moon_r*2.5),0,1)**2*70
glow=np.zeros((W,W,4),dtype=np.uint8); glow[...,0]=255; glow[...,1]=220; glow[...,2]=140; glow[...,3]=ga.astype(np.uint8)
img=Image.alpha_composite(img,Image.fromarray(glow,"RGBA"))
mm=Image.new("L",(W,W),0); md=ImageDraw.Draw(mm); md.ellipse([mx-moon_r,my-moon_r,mx+moon_r,my+moon_r],fill=255); off=int(58*S); md.ellipse([mx-moon_r+off,my-moon_r-off,mx+moon_r+off,my+moon_r-off],fill=0)
gt=np.array([0xFF,0xF8,0xDC],dtype=np.float32); gb=np.array([0xFF,0xC2,0x4E],dtype=np.float32); tg=np.clip((yy-(my-moon_r))/(2*moon_r),0,1)
gold=(gt[None,None,:]*(1-tg)[...,None]+gb[None,None,:]*tg[...,None]).astype(np.uint8)
ml=np.zeros((W,W,4),dtype=np.uint8); ml[...,:3]=gold; ml[...,3]=255; img.paste(Image.fromarray(ml,"RGBA"),(0,0),mm)
# stars
def star(d,x,y,r,c):
    pts=[]
    for i in range(8):
        a=math.pi/4*i; rr_=r if i%2==0 else r*0.36; pts.append((x+math.cos(a)*rr_,y+math.sin(a)*rr_))
    d.polygon(pts,fill=c)
sd=ImageDraw.Draw(img)
for sx,sy,sr in [(-265,-40,25),(-322,52,14),(-190,92,11)]:
    star(sd,cx+sx*S,lid_y+sy*S,sr*S,(255,244,204,245))
final=img.resize((1024,1024),Image.LANCZOS); out=os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "app", "Support", "AppIcon-1024.png"); final.save(out); print(out)
