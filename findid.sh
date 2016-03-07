echo -n `grep -i "$*" list.json -B1|grep appid|head -n1|sed -e 's/[^0-9]//g'`
